package opentunnel

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const maxConnections = 32

type Connection struct {
	client   *Client
	identity *identity
	routes   Routes
	ctx      context.Context
	cancel   context.CancelFunc
	ready    chan struct{}
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	cert     *tls.Certificate
	readyErr error
	err      error
	state    string
}

func (c *Client) Connect(ctx context.Context, routes Routes) (*Connection, error) {
	if len(routes) < 1 || len(routes) > 128 {
		return nil, errors.New("OpenTunnel requires between 1 and 128 explicitly enabled routes")
	}
	copyRoutes := make(Routes, len(routes))
	for name, dial := range routes {
		if (name != "@" && !label.MatchString(name)) || dial == nil {
			return nil, errors.New("invalid OpenTunnel route")
		}
		copyRoutes[name] = dial
	}
	value, pair, err := c.ensure(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	connection := &Connection{client: c, identity: value, cert: pair, routes: copyRoutes, ctx: ctx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}), state: "connecting"}
	go connection.run()
	return connection, nil
}

func (c *Connection) Ready(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ready:
		return c.readyErr
	}
}

func (c *Connection) URL(route string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return PublicURL(route, c.identity.Hostname)
}
func (c *Connection) Done() <-chan struct{} { return c.done }
func (c *Connection) Close()                { c.cancel(); <-c.done }

func (c *Connection) Status() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.err
}

func (c *Connection) signalReady(err error) {
	c.once.Do(func() { c.readyErr = err; close(c.ready) })
}

func (c *Connection) run() {
	defer close(c.done)
	refreshDone := make(chan struct{})
	go func() { defer close(refreshDone); c.renew() }()
	defer func() {
		c.cancel()
		<-refreshDone
		c.mu.Lock()
		c.state = "stopped"
		err := c.err
		c.mu.Unlock()
		if err == nil {
			err = c.ctx.Err()
		}
		c.signalReady(err)
	}()
	for attempt := 0; c.ctx.Err() == nil; attempt++ {
		attached, err := c.session()
		c.mu.Lock()
		c.err, c.state = err, "reconnecting"
		c.mu.Unlock()
		var refused *AttachError
		if errors.As(err, &refused) && (refused.Code == "bad_token" || refused.Code == "cert_not_ready" || refused.Code == "route_conflict" || refused.Code == "unsupported_protocol") {
			c.signalReady(err)
			return
		}
		if attached {
			attempt = 0
		}
		if pause(c.ctx, backoff(attempt, 30*time.Second)) != nil {
			return
		}
	}
}

func (c *Connection) renew() {
	for {
		c.mu.Lock()
		value := *c.identity
		expiry := c.cert.Leaf.NotAfter
		c.mu.Unlock()
		interval := 12 * time.Hour
		if time.Until(expiry) < 30*24*time.Hour {
			interval = time.Minute
		}
		if pause(c.ctx, interval) != nil {
			return
		}
		if c.client.lock(c.ctx) != nil {
			return
		}
		updated, pair, err := c.client.refresh(c.ctx, &value)
		c.client.unlock()
		if err == nil && pair != nil {
			c.mu.Lock()
			c.identity, c.cert = updated, pair
			c.mu.Unlock()
		}
	}
}

type control struct {
	Type          string   `json:"type"`
	Session       string   `json:"session"`
	Routes        []string `json:"routes"`
	Code          string   `json:"code"`
	Conn          uint32   `json:"conn"`
	SNI           string   `json:"sni"`
	HeartbeatMS   int64    `json:"heartbeat_ms"`
	IdleTimeoutMS int64    `json:"idle_timeout_ms"`
	TimeSent      int64    `json:"time_sent"`
}

type session struct {
	ctx      context.Context
	cancel   context.CancelFunc
	socket   *websocket.Conn
	controls chan []byte
	data     chan outbound
	mu       sync.Mutex
	streams  map[uint32]*stream
	workers  sync.WaitGroup
}

func (s *session) control(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case s.controls <- data:
		return nil
	default:
		s.cancel()
		return errControlQueue
	}
}

func (c *Connection) session() (bool, error) {
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	c.mu.Lock()
	id, hostname, token := c.identity.ID, c.identity.Hostname, c.identity.token
	c.mu.Unlock()
	attachCtx, attachCancel := context.WithTimeout(ctx, 10*time.Second)
	defer attachCancel()
	socket, _, err := websocket.Dial(attachCtx, c.client.api.Scheme+"://"+c.client.api.Host+"/api/tunnel/"+id+"/connect", &websocket.DialOptions{
		HTTPClient: c.client.http, Subprotocols: []string{"opentunnel"},
	})
	if err != nil {
		return false, errors.New("OpenTunnel bridge connection failed")
	}
	defer socket.CloseNow()
	if socket.Subprotocol() != "opentunnel" {
		return false, &AttachError{Code: "unsupported_protocol"}
	}
	socket.SetReadLimit(64 * 1024)
	names := make([]string, 0, len(c.routes))
	for name := range c.routes {
		names = append(names, name)
	}
	sort.Strings(names)
	attach, _ := json.Marshal(map[string]any{"type": "attach", "token": token, "transport": "ws", "routes": names, "client": map[string]any{"version": "incus.ssh/1", "max_conns": maxConnections}})
	if socket.Write(attachCtx, websocket.MessageText, attach) != nil {
		return false, errors.New("OpenTunnel bridge attach failed")
	}
	var first control
	for {
		kind, data, err := socket.Read(attachCtx)
		if err != nil {
			return false, errors.New("OpenTunnel bridge attach timed out or closed")
		}
		if kind != websocket.MessageText || json.Unmarshal(data, &first) != nil {
			continue
		}
		if first.Type == "attach_error" {
			code := first.Code
			if !label.MatchString(code) && code != "bad_token" && code != "cert_not_ready" && code != "route_conflict" && code != "too_many_routes" {
				code = "unknown_error"
			}
			return false, &AttachError{Code: code}
		}
		if first.Type == "attached" {
			break
		}
	}
	attachCancel()
	if first.Session == "" || first.HeartbeatMS <= 0 || first.IdleTimeoutMS <= 0 || len(first.Routes) != len(names) {
		return false, errors.New("invalid OpenTunnel attached response")
	}
	accepted := append([]string{}, first.Routes...)
	sort.Strings(accepted)
	for i := range names {
		if accepted[i] != names[i] {
			return false, errors.New("OpenTunnel attached a different route set")
		}
	}
	socket.SetReadLimit(maxPayload + 4)
	s := &session{ctx: ctx, cancel: cancel, socket: socket, controls: make(chan []byte, 128), data: make(chan outbound, 32), streams: make(map[uint32]*stream)}
	s.workers.Add(1)
	go s.writer()
	defer func() {
		cancel()
		socket.CloseNow()
		s.mu.Lock()
		for _, stream := range s.streams {
			stream.Close()
		}
		s.mu.Unlock()
		s.workers.Wait()
	}()
	c.mu.Lock()
	c.state, c.err = "connected", nil
	c.mu.Unlock()
	c.signalReady(nil)
	heartbeat := time.NewTicker(time.Duration(max(1000, min(first.HeartbeatMS, 60000))) * time.Millisecond)
	defer heartbeat.Stop()
	idle := time.Duration(max(5000, min(first.IdleTimeoutMS, 180000))) * time.Millisecond
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				s.control(map[string]any{"type": "ping", "time_sent": time.Now().UnixMilli()})
			}
		}
	}()
	for {
		readCtx, readCancel := context.WithTimeout(ctx, idle)
		kind, data, err := socket.Read(readCtx)
		readCancel()
		if err != nil {
			return true, errors.New("OpenTunnel bridge disconnected or idle")
		}
		if kind == websocket.MessageBinary {
			if len(data) < 4 || len(data) > maxPayload+4 {
				return true, errors.New("invalid OpenTunnel data frame")
			}
			s.deliver(binary.BigEndian.Uint32(data), packet{data: data[4:]})
			continue
		}
		var message control
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		switch message.Type {
		case "ping":
			s.control(map[string]any{"type": "pong", "time_sent": message.TimeSent})
		case "drain":
			return true, errors.New("OpenTunnel relay draining")
		case "open":
			route := routeForSNI(message.SNI, hostname)
			c.mu.Lock()
			pair := c.cert
			c.mu.Unlock()
			s.open(message.Conn, message.SNI, route, c.routes[route], pair)
		case "end":
			s.deliver(message.Conn, packet{end: true})
		case "reset":
			s.mu.Lock()
			if stream := s.streams[message.Conn]; stream != nil {
				stream.Close()
			}
			s.mu.Unlock()
		}
	}
}

func (s *session) reset(id uint32, code string) {
	s.control(map[string]any{"type": "reset", "conn": id, "code": code})
}

func (s *session) deliver(id uint32, item packet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stream := s.streams[id]
	if stream == nil {
		return
	}
	if !stream.push(item) {
		stream.Close()
		s.reset(id, "upstream_io_error")
	}
}

func (s *session) open(id uint32, sni, route string, dial DialTarget, pair *tls.Certificate) {
	if route == "" || dial == nil || !time.Now().Before(pair.Leaf.NotAfter) {
		s.reset(id, "unknown_route")
		return
	}
	s.mu.Lock()
	if previous := s.streams[id]; previous != nil {
		previous.Close()
		s.mu.Unlock()
		s.reset(id, "upstream_io_error")
		return
	}
	if len(s.streams) >= maxConnections {
		s.mu.Unlock()
		s.reset(id, "too_many_connections")
		return
	}
	raw := newStream(s, id)
	s.streams[id] = raw
	s.mu.Unlock()
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer func() {
			raw.Close()
			s.mu.Lock()
			delete(s.streams, id)
			s.mu.Unlock()
		}()
		secure := tls.Server(raw, &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if !strings.EqualFold(hello.ServerName, sni) {
					return nil, errors.New("TLS server name does not match the enabled route")
				}
				return pair, nil
			},
		})
		handshakeCtx, cancel := context.WithTimeout(raw.ctx, 10*time.Second)
		err := secure.HandshakeContext(handshakeCtx)
		cancel()
		if err != nil {
			s.reset(id, "upstream_io_error")
			return
		}
		dialCtx, cancel := context.WithTimeout(raw.ctx, 10*time.Second)
		upstream, err := dial(dialCtx)
		cancel()
		if err != nil {
			s.reset(id, "upstream_connect_failed")
			return
		}
		defer upstream.Close()
		stop := context.AfterFunc(raw.ctx, func() { upstream.Close() })
		defer stop()
		finished := make(chan error, 1)
		go func() {
			_, err := io.Copy(upstream, secure)
			if err == nil {
				if half, ok := upstream.(interface{ CloseWrite() error }); ok {
					err = half.CloseWrite()
				}
			}
			if err != nil {
				raw.Close()
				upstream.Close()
			}
			finished <- err
		}()
		_, err = io.Copy(secure, upstream)
		if err == nil {
			err = secure.CloseWrite()
			if err == nil {
				err = raw.CloseWrite()
			}
		}
		if err != nil {
			raw.Close()
			upstream.Close()
		}
		if other := <-finished; err != nil || other != nil {
			s.reset(id, "upstream_io_error")
		}
	}()
}

var _ net.Conn = (*stream)(nil)
