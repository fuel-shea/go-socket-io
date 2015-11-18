package socketio

import (
	"crypto/rand"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	SessionIDLength  = 16
	SessionIDCharset = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

var NotConnected = errors.New("not connected")

var sessionPool = sync.Pool{
	New: func() interface{} {
		return &Session{}
	},
}

type Session struct {
	SessionId         string
	mutex             sync.Mutex
	emitters          map[string]*EventEmitter
	nameSpaces        map[string]*NameSpace
	transport         Transport
	heartbeatTimeout  time.Duration
	connectionTimeout time.Duration
	peerLast          time.Time
	lastCheck         time.Time
	sendHeartBeat     bool
	defaultNS         *NameSpace
	Values            map[interface{}]interface{}
	Request           *http.Request
	srv               *SocketIOServer
	didConnect        bool
}

func NewSessionID() string {
	b := make([]byte, SessionIDLength)

	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return ""
	}

	for i := 0; i < SessionIDLength; i++ {
		b[i] = SessionIDCharset[b[i]%uint8(len(SessionIDCharset))]
	}

	return string(b)
}

func NewSession(emitters map[string]*EventEmitter, sessionId string, timeout int, sendHeartbeat bool, r *http.Request, srv *SocketIOServer) *Session {
	ss := &Session{
		emitters:          emitters,
		SessionId:         sessionId,
		nameSpaces:        make(map[string]*NameSpace),
		sendHeartBeat:     sendHeartbeat,
		heartbeatTimeout:  time.Duration(timeout) * time.Second,
		connectionTimeout: time.Duration(timeout*2/3) * time.Second,
		Values:            make(map[interface{}]interface{}),
		Request:           r,
		srv:               srv,
	}
	ss.defaultNS = ss.Of("")

	go ss.killIfStalled()

	return ss
}

func (ss *Session) Of(name string) (nameSpace *NameSpace) {
	ss.mutex.Lock()
	defer ss.mutex.Unlock()
	if nameSpace = ss.nameSpaces[name]; nameSpace == nil {
		ee := ss.emitters[name]
		if ee == nil {
			ss.emitters[name] = NewEventEmitter()
			ee = ss.emitters[name]
		}
		nameSpace = NewNameSpace(ss, name, ee)
		ss.nameSpaces[name] = nameSpace
	}
	return
}

func (ss *Session) Disconnect() error {
	for _, ns := range ss.nameSpaces {
		if err := ns.Disconnect(); err != nil {
			return err
		}
	}
	return nil
}

func (ss *Session) cleanup() {
	if ss.srv != nil {
		ss.srv.removeSession(ss)
	}
	ss.nameSpaces = map[string]*NameSpace{}
	ss.Request = nil
	if ss.transport != nil {
		ss.transport.Close()
	}
}

func (ss *Session) loop() {
	ss.didConnect = true

	err := ss.onOpen()
	if err != nil {
		return
	}
	defer func() {
		for _, ns := range ss.nameSpaces {
			ns.onDisconnect()
		}
		ss.cleanup()
	}()

	for {
		if err := ss.checkConnection(); err != nil {
			return
		}

		packet, err := ss.getPacket()
		if err != nil {
			return
		}
		if packet == nil {
			continue
		}

		if packet.EndPoint() == "" {
			if err := ss.onPacket(packet); err != nil {
				return
			}
		}

		ns := ss.Of(packet.EndPoint())
		if ns == nil {
			continue
		}
		ns.onPacket(packet)
	}
}

func (ss *Session) checkConnection() error {
	now := time.Now()
	if ss.sendHeartBeat && now.Sub(ss.lastCheck) > ss.heartbeatTimeout {
		hb := new(heartbeatPacket)
		if err := ss.defaultNS.sendPacket(hb); err != nil {
			return err
		}
		ss.lastCheck = now
	}
	if now.Sub(ss.peerLast) > ss.connectionTimeout {
		return NotConnected
	}
	return nil
}

func (ss *Session) getPacket() (Packet, error) {
	reader, err := ss.transport.Read()
	if e, ok := err.(net.Error); ok && e.Timeout() {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, err
	}

	return decodePacket(b)
}

func (ss *Session) onPacket(packet Packet) error {
	switch packet.(type) {
	case *heartbeatPacket:
		ss.peerLast = time.Now()
		if !ss.sendHeartBeat {
			err := ss.defaultNS.sendPacket(new(heartbeatPacket))
			if err != nil {
				return err
			}
			ss.lastCheck = time.Now()
		}
	case *disconnectPacket:
		ss.defaultNS.onDisconnect()
		return NotConnected
	}
	return nil
}

func (ss *Session) onOpen() error {
	packet := new(connectPacket)
	ss.defaultNS.connected = true
	err := ss.defaultNS.sendPacket(packet)
	ss.defaultNS.emit("connect", ss.defaultNS, nil)
	ss.lastCheck, ss.peerLast = time.Now(), time.Now()
	return err
}

func (ss *Session) killIfStalled() {
	// Of course, this is not ideal.
	// But we are moving away from 0.9.x, so we're
	// accpeting the easiest way to prune zombie sessions.
	loopUntil := time.Now().Add(5 * time.Second)
	for time.Now().Before(loopUntil) {
		time.Sleep(50 * time.Millisecond)
		if ss.didConnect {
			return
		}
	}
	ss.cleanup()
}
