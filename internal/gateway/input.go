package gateway

import (
	"context"
	"io"
	"sync"
)

const maxInputBuffer = 32 * 1024

type sessionInput struct {
	ctx     context.Context
	mu      sync.Mutex
	buffer  []byte
	err     error
	changed chan struct{}
	done    chan struct{}
}

func newSessionInput(ctx context.Context, source io.Reader) *sessionInput {
	input := &sessionInput{ctx: ctx, changed: make(chan struct{}), done: make(chan struct{})}
	go input.pump(source)
	return input
}

func (r *sessionInput) notify() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *sessionInput) pump(source io.Reader) {
	defer close(r.done)
	var data [8192]byte
	for {
		r.mu.Lock()
		space := maxInputBuffer - len(r.buffer)
		changed := r.changed
		r.mu.Unlock()
		if r.ctx.Err() != nil {
			return
		}
		if space == 0 {
			select {
			case <-r.ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		if space > len(data) {
			space = len(data)
		}
		n, err := source.Read(data[:space])
		r.mu.Lock()
		r.buffer = append(r.buffer, data[:n]...)
		r.err = err
		r.notify()
		r.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (r *sessionInput) Read(data []byte) (int, error) {
	return r.ReadContext(r.ctx, data)
}

func (r *sessionInput) ReadContext(ctx context.Context, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	for {
		r.mu.Lock()
		if err := ctx.Err(); err != nil {
			r.mu.Unlock()
			return 0, err
		}
		if err := r.ctx.Err(); err != nil {
			r.mu.Unlock()
			return 0, err
		}
		if len(r.buffer) > 0 {
			n := copy(data, r.buffer)
			r.buffer = r.buffer[n:]
			r.notify()
			r.mu.Unlock()
			return n, nil
		}
		if r.err != nil {
			err := r.err
			r.mu.Unlock()
			return 0, err
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-changed:
		}
	}
}
