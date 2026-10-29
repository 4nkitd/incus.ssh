package opentunnel

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestReceiveBudgetCountsBytesInsteadOfFrames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raw := newStream(&session{ctx: ctx}, 1)
	defer raw.Close()
	part := bytes.Repeat([]byte{0, 255, 1, 128}, 64)
	for range maxPending / len(part) {
		if !raw.push(packet{data: part}) {
			t.Fatal("small frames exhausted a frame-count limit despite available byte budget")
		}
	}
	if raw.push(packet{data: []byte{1}}) {
		t.Fatal("receive buffer exceeded its byte budget")
	}
	if !raw.push(packet{end: true}) {
		t.Fatal("half-close consumed receive byte budget")
	}
	data, err := io.ReadAll(raw)
	if err != nil || !bytes.Equal(data, bytes.Repeat(part, maxPending/len(part))) {
		t.Fatal("frame coalescing changed stream bytes or half-close ordering", err)
	}
}
