package gateway

import (
	"context"
	"sync"

	"incus-for-devs/internal/core"
)

type commandEvents struct {
	resize  chan core.Window
	signals chan string
}

type sessionHandler struct {
	handler core.Handler
	mu      sync.Mutex
	window  core.Window
	active  *commandEvents
}

func (h *sessionHandler) begin(streams core.Streams) (core.Streams, func()) {
	h.mu.Lock()
	events := &commandEvents{resize: make(chan core.Window, 1), signals: make(chan string, 8)}
	h.active = events
	streams.Window, streams.Resize, streams.Signals = h.window, events.resize, events.signals
	h.mu.Unlock()
	return streams, func() {
		h.mu.Lock()
		h.active = nil
	signals:
		for {
			select {
			case <-events.signals:
			default:
				break signals
			}
		}
	resize:
		for {
			select {
			case <-events.resize:
			default:
				break resize
			}
		}
		h.mu.Unlock()
	}
}

func (h *sessionHandler) Handle(ctx context.Context, principal core.Principal, command string, streams core.Streams) int {
	streams, finish := h.begin(streams)
	defer finish()
	return h.handler.Handle(ctx, principal, command, streams)
}

func (h *sessionHandler) resize(window core.Window) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// SSH clients without a local terminal send zero for unspecified dimensions.
	if window.Width == 0 {
		window.Width = h.window.Width
		if window.Width == 0 {
			window.Width = 80
		}
	}
	if window.Height == 0 {
		window.Height = h.window.Height
		if window.Height == 0 {
			window.Height = 24
		}
	}
	h.window = window
	if h.active != nil {
		select {
		case <-h.active.resize:
		default:
		}
		select {
		case h.active.resize <- window:
		default:
		}
	}
}

func (h *sessionHandler) signal(signal string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.active == nil {
		return false
	}
	select {
	case h.active.signals <- signal:
		return true
	default:
		return false
	}
}
