package opentunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (r roundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return r(request) }

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCreationRetriesOnlyBeforeHTTPTransmissionIsPossible(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		connected bool
		calls     int
	}{
		{"nested proxy dial", &net.OpError{Op: "proxyconnect", Err: &net.OpError{Op: "dial", Err: errors.New("unreachable proxy")}}, false, 2},
		{"TLS setup EOF", io.ErrUnexpectedEOF, false, 2},
		{"established connection EOF", io.ErrUnexpectedEOF, true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				if test.connected {
					httptrace.ContextClientTrace(req.Context()).GotConn(httptrace.GotConnInfo{})
				}
				if calls == 1 {
					return nil, test.err
				}
				return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
			})}
			c, err := New(Options{API: "https://api.test", Directory: filepath.Join(t.TempDir(), "identity"), HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			err = c.request(context.Background(), "POST", "/api/tunnel", "", struct{}{}, nil, false)
			if calls != test.calls || (err != nil) != test.connected {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestSuccessfulResponseReadFailureFollowsReplayPolicy(t *testing.T) {
	for _, idempotent := range []bool{false, true} {
		calls := 0
		client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
			calls++
			body := io.NopCloser(io.Reader(strings.NewReader(`{}`)))
			if calls == 1 {
				body = io.NopCloser(brokenBody{})
			}
			return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
		})}
		c, err := New(Options{API: "https://api.test", Directory: filepath.Join(t.TempDir(), "identity"), HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		err = c.request(context.Background(), "GET", "/api/tunnel/demo/certificate", "", nil, nil, idempotent)
		c.Close()
		if idempotent && (calls != 2 || err != nil) {
			t.Fatal("idempotent response read was not retried", calls, err)
		}
		if !idempotent && (calls != 1 || err == nil) {
			t.Fatal("ambiguous non-idempotent response was replayed", calls, err)
		}
	}
}
