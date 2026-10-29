package opentunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const maxPayload = 32 * 1024
const maxPending = 256 * 1024

type packet struct {
	data []byte
	end  bool
}

type stream struct {
	ctx      context.Context
	cancel   context.CancelFunc
	session  *session
	id       uint32
	incoming bytes.Buffer
	wake     chan struct{}
	eof      bool
	mu       sync.Mutex
	readBy   time.Time
	writeBy  time.Time
	changed  chan struct{}
}

func newStream(s *session, id uint32) *stream {
	ctx, cancel := context.WithCancel(s.ctx)
	return &stream{ctx: ctx, cancel: cancel, session: s, id: id, wake: make(chan struct{}, 1), changed: make(chan struct{})}
}

func (s *stream) push(item packet) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return true
	}
	if len(item.data) > maxPending-s.incoming.Len() || (s.eof && len(item.data) != 0) {
		return false
	}
	s.incoming.Write(item.data)
	s.eof = s.eof || item.end
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

func (s *stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if s.ctx.Err() != nil {
			return 0, net.ErrClosed
		}
		s.mu.Lock()
		deadline, changed := s.readBy, s.changed
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			s.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if s.incoming.Len() > 0 {
			n, err := s.incoming.Read(p)
			s.mu.Unlock()
			return n, err
		}
		if s.eof {
			s.mu.Unlock()
			return 0, io.EOF
		}
		s.mu.Unlock()
		var timeout <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case <-s.ctx.Done():
		case <-changed:
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-s.wake:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *stream) Write(p []byte) (int, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	s.mu.Lock()
	deadline := s.writeBy
	s.mu.Unlock()
	if !deadline.IsZero() {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, deadline)
		defer deadlineCancel()
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxPayload)
		frame := make([]byte, n+4)
		binary.BigEndian.PutUint32(frame, s.id)
		copy(frame[4:], p[:n])
		if err := s.session.sendData(ctx, frame); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (s *stream) Close() error { s.cancel(); return nil }

func (s *stream) CloseWrite() error {
	return s.session.control(map[string]any{"type": "end", "conn": s.id})
}

type streamAddress string

func (a streamAddress) Network() string { return "opentunnel" }
func (a streamAddress) String() string  { return string(a) }
func (s *stream) LocalAddr() net.Addr   { return streamAddress("client") }
func (s *stream) RemoteAddr() net.Addr  { return streamAddress("visitor") }

func (s *stream) SetDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readBy, s.writeBy = t, t
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *stream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readBy = t
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *stream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeBy = t
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

type outbound struct {
	data []byte
	done chan error
}

func (s *session) sendData(ctx context.Context, data []byte) error {
	frame := outbound{data: data, done: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return net.ErrClosed
	case s.data <- frame:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return net.ErrClosed
	case err := <-frame.done:
		return err
	}
}

func (s *session) writer() {
	defer s.workers.Done()
	for {
		var control []byte
		var frame outbound
		select {
		case <-s.ctx.Done():
			return
		case control = <-s.controls:
		default:
			select {
			case <-s.ctx.Done():
				return
			case control = <-s.controls:
			case frame = <-s.data:
			}
		}
		kind, data := websocket.MessageText, control
		if control == nil {
			kind, data = websocket.MessageBinary, frame.data
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		err := s.socket.Write(ctx, kind, data)
		cancel()
		if frame.done != nil {
			frame.done <- err
		}
		if err != nil {
			s.cancel()
			return
		}
	}
}

var errControlQueue = errors.New("OpenTunnel control queue exhausted")
