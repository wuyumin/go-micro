package tunnel

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/micro/go-micro/transport"
	"github.com/micro/go-micro/util/log"
)

var (
	// KeepAliveTime defines time interval we send keepalive messages to outbound links
	KeepAliveTime = 30 * time.Second
	// ReconnectTime defines time interval we periodically attempt to reconnect dead links
	ReconnectTime = 5 * time.Second
)

// tun represents a network tunnel
type tun struct {
	options Options

	sync.RWMutex

	// the unique id for this tunnel
	id string

	// tunnel token for authentication
	token string

	// to indicate if we're connected or not
	connected bool

	// the send channel for all messages
	send chan *message

	// close channel
	closed chan bool

	// a map of sessions based on Micro-Tunnel-Channel
	sessions map[string]*session

	// outbound links
	links map[string]*link

	// listener
	listener transport.Listener
}

// create new tunnel on top of a link
func newTunnel(opts ...Option) *tun {
	options := DefaultOptions()
	for _, o := range opts {
		o(&options)
	}

	return &tun{
		options:  options,
		id:       options.Id,
		token:    options.Token,
		send:     make(chan *message, 128),
		closed:   make(chan bool),
		sessions: make(map[string]*session),
		links:    make(map[string]*link),
	}
}

// Init initializes tunnel options
func (t *tun) Init(opts ...Option) error {
	t.Lock()
	defer t.Unlock()
	for _, o := range opts {
		o(&t.options)
	}
	return nil
}

// getSession returns a session from the internal session map.
// It does this based on the Micro-Tunnel-Channel and Micro-Tunnel-Session
func (t *tun) getSession(channel, session string) (*session, bool) {
	// get the session
	t.RLock()
	s, ok := t.sessions[channel+session]
	t.RUnlock()
	return s, ok
}

// newSession creates a new session and saves it
func (t *tun) newSession(channel, sessionId string) (*session, bool) {
	// new session
	s := &session{
		id:      t.id,
		channel: channel,
		session: sessionId,
		closed:  make(chan bool),
		recv:    make(chan *message, 128),
		send:    t.send,
		wait:    make(chan bool),
		errChan: make(chan error, 1),
	}

	// save session
	t.Lock()
	_, ok := t.sessions[channel+sessionId]
	if ok {
		// session already exists
		t.Unlock()
		return nil, false
	}

	t.sessions[channel+sessionId] = s
	t.Unlock()

	// return session
	return s, true
}

// TODO: use tunnel id as part of the session
func (t *tun) newSessionId() string {
	return uuid.New().String()
}

// monitor monitors outbound links and attempts to reconnect to the failed ones
func (t *tun) monitor() {
	reconnect := time.NewTicker(ReconnectTime)
	defer reconnect.Stop()

	for {
		select {
		case <-t.closed:
			return
		case <-reconnect.C:
			var connect []string

			// build list of unknown nodes to connect to
			t.RLock()
			for _, node := range t.options.Nodes {
				if _, ok := t.links[node]; !ok {
					connect = append(connect, node)
				}
			}
			t.RUnlock()

			for _, node := range connect {
				// create new link
				link, err := t.setupLink(node)
				if err != nil {
					log.Debugf("Tunnel failed to setup node link to %s: %v", node, err)
					continue
				}

				// save the link
				t.Lock()
				t.links[node] = link
				t.Unlock()
			}
		}
	}
}

// process outgoing messages sent by all local sessions
func (t *tun) process() {
	// manage the send buffer
	// all pseudo sessions throw everything down this
	for {
		select {
		case msg := <-t.send:
			newMsg := &transport.Message{
				Header: make(map[string]string),
				Body:   msg.data.Body,
			}

			for k, v := range msg.data.Header {
				newMsg.Header[k] = v
			}

			// set message head
			newMsg.Header["Micro-Tunnel"] = msg.typ

			// set the tunnel id on the outgoing message
			newMsg.Header["Micro-Tunnel-Id"] = msg.id

			// set the tunnel channel on the outgoing message
			newMsg.Header["Micro-Tunnel-Channel"] = msg.channel

			// set the session id
			newMsg.Header["Micro-Tunnel-Session"] = msg.session

			// set the tunnel token
			newMsg.Header["Micro-Tunnel-Token"] = t.token

			// send the message via the interface
			t.Lock()

			if len(t.links) == 0 {
				log.Debugf("No links to send to")
			}

			var sent bool
			var err error

			for node, link := range t.links {
				// if the link is not connected skip it
				if !link.connected {
					log.Debugf("Link for node %s not connected", node)
					err = errors.New("link not connected")
					continue
				}

				// if we're picking the link check the id
				// this is where we explicitly set the link
				// in a message received via the listen method
				if len(msg.link) > 0 && link.id != msg.link {
					err = errors.New("link not found")
					continue
				}

				// if the link was a loopback accepted connection
				// and the message is being sent outbound via
				// a dialled connection don't use this link
				if link.loopback && msg.outbound {
					err = errors.New("link is loopback")
					continue
				}

				// if the message was being returned by the loopback listener
				// send it back up the loopback link only
				if msg.loopback && !link.loopback {
					err = errors.New("link is not loopback")
					continue
				}

				// send the message via the current link
				log.Debugf("Sending %+v to %s", newMsg, node)
				if errr := link.Send(newMsg); errr != nil {
					log.Debugf("Tunnel error sending %+v to %s: %v", newMsg, node, errr)
					err = errors.New(errr.Error())
					delete(t.links, node)
					continue
				}
				// is sent
				sent = true
			}

			t.Unlock()

			var gerr error
			if !sent {
				gerr = err
			}

			// return error non blocking
			select {
			case msg.errChan <- gerr:
			default:
			}
		case <-t.closed:
			return
		}
	}
}

// process incoming messages
func (t *tun) listen(link *link) {
	// remove the link on exit
	defer func() {
		log.Debugf("Tunnel deleting connection from %s", link.Remote())
		t.Lock()
		delete(t.links, link.Remote())
		t.Unlock()
	}()

	// let us know if its a loopback
	var loopback bool

	for {
		// process anything via the net interface
		msg := new(transport.Message)
		if err := link.Recv(msg); err != nil {
			log.Debugf("Tunnel link %s receive error: %#v", link.Remote(), err)
			return
		}

		// always ensure we have the correct auth token
		// TODO: segment the tunnel based on token
		// e.g use it as the basis
		token := msg.Header["Micro-Tunnel-Token"]
		if token != t.token {
			log.Debugf("Tunnel link %s received invalid token %s", token)
			return
		}

		switch msg.Header["Micro-Tunnel"] {
		case "connect":
			log.Debugf("Tunnel link %s received connect message", link.Remote())

			id := msg.Header["Micro-Tunnel-Id"]

			// are we connecting to ourselves?
			if id == t.id {
				link.loopback = true
				loopback = true
			}

			// set as connected
			link.connected = true

			// save the link once connected
			t.Lock()
			t.links[link.Remote()] = link
			t.Unlock()

			// nothing more to do
			continue
		case "close":
			log.Debugf("Tunnel link %s closing connection", link.Remote())
			// TODO: handle the close message
			// maybe report io.EOF or kill the link
			return
		case "keepalive":
			log.Debugf("Tunnel link %s received keepalive", link.Remote())
			t.Lock()
			// save the keepalive
			link.lastKeepAlive = time.Now()
			t.Unlock()
			continue
		case "message":
			// process message
			log.Debugf("Received %+v from %s", msg, link.Remote())
		default:
			// blackhole it
			continue
		}

		// if its not connected throw away the link
		if !link.connected {
			log.Debugf("Tunnel link %s not connected", link.id)
			return
		}

		// the tunnel id
		id := msg.Header["Micro-Tunnel-Id"]
		// the tunnel channel
		channel := msg.Header["Micro-Tunnel-Channel"]
		// the session id
		sessionId := msg.Header["Micro-Tunnel-Session"]

		// strip tunnel message header
		for k, _ := range msg.Header {
			if strings.HasPrefix(k, "Micro-Tunnel") {
				delete(msg.Header, k)
			}
		}

		// if the session id is blank there's nothing we can do
		// TODO: check this is the case, is there any reason
		// why we'd have a blank session? Is the tunnel
		// used for some other purpose?
		if len(channel) == 0 || len(sessionId) == 0 {
			continue
		}

		var s *session
		var exists bool

		// If its a loopback connection then we've enabled link direction
		// listening side is used for listening, the dialling side for dialling
		switch {
		case loopback:
			s, exists = t.getSession(channel, "listener")
		default:
			// get the session based on the tunnel id and session
			// this could be something we dialed in which case
			// we have a session for it otherwise its a listener
			s, exists = t.getSession(channel, sessionId)
			if !exists {
				// try get it based on just the tunnel id
				// the assumption here is that a listener
				// has no session but its set a listener session
				s, exists = t.getSession(channel, "listener")
			}
		}

		// bail if no session has been found
		if !exists {
			log.Debugf("Tunnel skipping no session exists")
			// drop it, we don't care about
			// messages we don't know about
			continue
		}

		log.Debugf("Tunnel using session %s %s", s.channel, s.session)

		// is the session closed?
		select {
		case <-s.closed:
			// closed
			delete(t.sessions, channel)
			continue
		default:
			// process
		}

		// is the session new?
		select {
		// if its new the session is actually blocked waiting
		// for a connection. so we check if its waiting.
		case <-s.wait:
		// if its waiting e.g its new then we close it
		default:
			// set remote address of the session
			s.remote = msg.Header["Remote"]
			close(s.wait)
		}

		// construct a new transport message
		tmsg := &transport.Message{
			Header: msg.Header,
			Body:   msg.Body,
		}

		// construct the internal message
		imsg := &message{
			id:       id,
			channel:  channel,
			session:  sessionId,
			data:     tmsg,
			link:     link.id,
			loopback: loopback,
			errChan:  make(chan error, 1),
		}

		// append to recv backlog
		// we don't block if we can't pass it on
		select {
		case s.recv <- imsg:
		default:
		}
	}
}

// keepalive periodically sends keepalive messages to link
func (t *tun) keepalive(link *link) {
	keepalive := time.NewTicker(KeepAliveTime)
	defer keepalive.Stop()

	for {
		select {
		case <-t.closed:
			return
		case <-keepalive.C:
			// send keepalive message
			log.Debugf("Tunnel sending keepalive to link: %v", link.Remote())
			if err := link.Send(&transport.Message{
				Header: map[string]string{
					"Micro-Tunnel":       "keepalive",
					"Micro-Tunnel-Id":    t.id,
					"Micro-Tunnel-Token": t.token,
				},
			}); err != nil {
				log.Debugf("Error sending keepalive to link %v: %v", link.Remote(), err)
				t.Lock()
				delete(t.links, link.Remote())
				t.Unlock()
				return
			}
		}
	}
}

// setupLink connects to node and returns link if successful
// It returns error if the link failed to be established
func (t *tun) setupLink(node string) (*link, error) {
	log.Debugf("Tunnel setting up link: %s", node)
	c, err := t.options.Transport.Dial(node)
	if err != nil {
		log.Debugf("Tunnel failed to connect to %s: %v", node, err)
		return nil, err
	}
	log.Debugf("Tunnel connected to %s", node)

	if err := c.Send(&transport.Message{
		Header: map[string]string{
			"Micro-Tunnel":       "connect",
			"Micro-Tunnel-Id":    t.id,
			"Micro-Tunnel-Token": t.token,
		},
	}); err != nil {
		return nil, err
	}

	// create a new link
	link := newLink(c)
	link.connected = true
	// we made the outbound connection
	// and sent the connect message

	// process incoming messages
	go t.listen(link)

	// start keepalive monitor
	go t.keepalive(link)

	return link, nil
}

// connect the tunnel to all the nodes and listen for incoming tunnel connections
func (t *tun) connect() error {
	l, err := t.options.Transport.Listen(t.options.Address)
	if err != nil {
		return err
	}

	// save the listener
	t.listener = l

	go func() {
		// accept inbound connections
		err := l.Accept(func(sock transport.Socket) {
			log.Debugf("Tunnel accepted connection from %s", sock.Remote())

			// create a new link
			link := newLink(sock)

			// listen for inbound messages.
			// only save the link once connected.
			// we do this inside liste
			t.listen(link)
		})

		t.RLock()
		defer t.RUnlock()

		// still connected but the tunnel died
		if err != nil && t.connected {
			log.Logf("Tunnel listener died: %v", err)
		}
	}()

	for _, node := range t.options.Nodes {
		// skip zero length nodes
		if len(node) == 0 {
			continue
		}

		// connect to node and return link
		link, err := t.setupLink(node)
		if err != nil {
			log.Debugf("Tunnel failed to establish node link to %s: %v", node, err)
			continue
		}

		// save the link
		t.links[node] = link
	}

	// process outbound messages to be sent
	// process sends to all links
	go t.process()

	// monitor links
	go t.monitor()

	return nil
}

// Connect the tunnel
func (t *tun) Connect() error {
	t.Lock()
	defer t.Unlock()

	// already connected
	if t.connected {
		return nil
	}

	// send the connect message
	if err := t.connect(); err != nil {
		return err
	}

	// set as connected
	t.connected = true
	// create new close channel
	t.closed = make(chan bool)

	return nil
}

func (t *tun) close() error {
	// close all the links
	for node, link := range t.links {
		link.Send(&transport.Message{
			Header: map[string]string{
				"Micro-Tunnel":       "close",
				"Micro-Tunnel-Id":    t.id,
				"Micro-Tunnel-Token": t.token,
			},
		})
		link.Close()
		delete(t.links, node)
	}

	// close the listener
	return t.listener.Close()
}

func (t *tun) Address() string {
	t.RLock()
	defer t.RUnlock()

	if !t.connected {
		return t.options.Address
	}

	return t.listener.Addr()
}

// Close the tunnel
func (t *tun) Close() error {
	t.Lock()
	defer t.Unlock()

	if !t.connected {
		return nil
	}

	select {
	case <-t.closed:
		return nil
	default:
		// close all the sessions
		for id, s := range t.sessions {
			s.Close()
			delete(t.sessions, id)
		}
		// close the connection
		close(t.closed)
		t.connected = false

		// send a close message
		// we don't close the link
		// just the tunnel
		return t.close()
	}

	return nil
}

// Dial an address
func (t *tun) Dial(channel string) (Session, error) {
	log.Debugf("Tunnel dialing %s", channel)
	c, ok := t.newSession(channel, t.newSessionId())
	if !ok {
		return nil, errors.New("error dialing " + channel)
	}
	// set remote
	c.remote = channel
	// set local
	c.local = "local"
	// outbound session
	c.outbound = true

	return c, nil
}

// Accept a connection on the address
func (t *tun) Listen(channel string) (Listener, error) {
	log.Debugf("Tunnel listening on %s", channel)
	// create a new session by hashing the address
	c, ok := t.newSession(channel, "listener")
	if !ok {
		return nil, errors.New("already listening on " + channel)
	}

	// set remote. it will be replaced by the first message received
	c.remote = "remote"
	// set local
	c.local = channel

	tl := &tunListener{
		channel: channel,
		// the accept channel
		accept: make(chan *session, 128),
		// the channel to close
		closed: make(chan bool),
		// tunnel closed channel
		tunClosed: t.closed,
		// the listener session
		session: c,
	}

	// this kicks off the internal message processor
	// for the listener so it can create pseudo sessions
	// per session if they do not exist or pass messages
	// to the existign sessions
	go tl.process()

	// return the listener
	return tl, nil
}

func (t *tun) String() string {
	return "mucp"
}
