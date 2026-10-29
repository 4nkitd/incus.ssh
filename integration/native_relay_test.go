package integration_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"incus-for-devs/internal/opentunnel"
)

type nativeRelay struct {
	server   *httptest.Server
	mu       sync.Mutex
	cert     string
	ca       *x509.Certificate
	key      *ecdsa.PrivateKey
	pool     *x509.CertPool
	routes   map[string]*nativeBridge
	next     atomic.Uint32
	sessions atomic.Int32
}

type nativeBridge struct {
	ctx   context.Context
	ws    *websocket.Conn
	mu    sync.Mutex
	pipes map[uint32]net.Conn
}

func newNativeRelay(t *testing.T) *nativeRelay {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "native QA CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	r := &nativeRelay{ca: ca, key: key, pool: x509.NewCertPool(), routes: make(map[string]*nativeBridge)}
	r.pool.AddCert(ca)
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

func (r *nativeRelay) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/api/tunnel/qa/connect" {
		r.bridge(w, req)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path == "/api/tunnel" && req.Method == "POST" {
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"tunnel": map[string]string{"id": "qa", "hostname": "qa.test"}, "token": "qa-synthetic-token"})
		return
	}
	if req.URL.Path != "/api/tunnel/qa/certificate" || req.Header.Get("Authorization") != "Bearer qa-synthetic-token" {
		w.WriteHeader(401)
		return
	}
	if req.Method == "POST" {
		var body struct {
			CSR string `json:"csr"`
		}
		if json.NewDecoder(req.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		block, _ := pem.Decode([]byte(body.CSR))
		if block == nil {
			w.WriteHeader(400)
			return
		}
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil || csr.CheckSignature() != nil {
			w.WriteHeader(400)
			return
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(60 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, r.ca, csr.PublicKey, r.key)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		r.cert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		w.WriteHeader(202)
		io.WriteString(w, `{"state":{"type":"issuing"}}`)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"state": map[string]string{"type": "ready", "certificate": r.cert, "chain": ""}})
}

func (r *nativeRelay) bridge(w http.ResponseWriter, req *http.Request) {
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{"opentunnel"}})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(64 * 1024)
	_, data, err := ws.Read(req.Context())
	if err != nil {
		return
	}
	var attach struct {
		Token  string   `json:"token"`
		Routes []string `json:"routes"`
	}
	if json.Unmarshal(data, &attach) != nil || attach.Token != "qa-synthetic-token" {
		return
	}
	b := &nativeBridge{ctx: req.Context(), ws: ws, pipes: make(map[uint32]net.Conn)}
	r.sessions.Add(1)
	r.mu.Lock()
	for _, route := range attach.Routes {
		r.routes[route] = b
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		for _, route := range attach.Routes {
			if r.routes[route] == b {
				delete(r.routes, route)
			}
		}
		r.mu.Unlock()
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, pipe := range b.pipes {
			pipe.Close()
		}
	}()
	attached, _ := json.Marshal(map[string]any{"type": "attached", "session": "native-session", "routes": attach.Routes, "heartbeat_ms": 1000, "idle_timeout_ms": 5000})
	if ws.Write(req.Context(), websocket.MessageText, attached) != nil {
		return
	}
	for {
		kind, data, err := ws.Read(req.Context())
		if err != nil {
			return
		}
		if kind == websocket.MessageBinary {
			if len(data) < 4 {
				return
			}
			b.mu.Lock()
			pipe := b.pipes[binary.BigEndian.Uint32(data)]
			b.mu.Unlock()
			if pipe != nil {
				pipe.SetWriteDeadline(time.Now().Add(5 * time.Second))
				pipe.Write(data[4:])
			}
			continue
		}
		var control struct {
			Type string `json:"type"`
			Conn uint32 `json:"conn"`
			Time int64  `json:"time_sent"`
		}
		if json.Unmarshal(data, &control) != nil {
			return
		}
		if control.Type == "ping" {
			payload, _ := json.Marshal(map[string]any{"type": "pong", "time_sent": control.Time})
			ws.Write(req.Context(), websocket.MessageText, payload)
		}
		if control.Type == "end" || control.Type == "reset" {
			b.mu.Lock()
			if pipe := b.pipes[control.Conn]; pipe != nil {
				pipe.Close()
				delete(b.pipes, control.Conn)
			}
			b.mu.Unlock()
		}
	}
}

func (r *nativeRelay) visit(publicURL string) (string, error) {
	u, err := url.Parse(publicURL)
	if err != nil {
		return "", err
	}
	route, ok := strings.CutSuffix(u.Hostname(), ".qa.test")
	if !ok {
		return "", errors.New("invalid QA hostname")
	}
	r.mu.Lock()
	b := r.routes[route]
	r.mu.Unlock()
	if b == nil {
		return "", errors.New("route is private")
	}
	id := r.next.Add(1)
	left, right := net.Pipe()
	b.mu.Lock()
	b.pipes[id] = right
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open, _ := json.Marshal(map[string]any{"type": "open", "conn": id, "sni": u.Hostname(), "peer": "192.0.2.1:54321", "alpn": ""})
	if err := b.ws.Write(ctx, websocket.MessageText, open); err != nil {
		left.Close()
		right.Close()
		b.mu.Lock()
		delete(b.pipes, id)
		b.mu.Unlock()
		return "", err
	}
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		defer right.Close()
		data := make([]byte, 32768)
		for {
			n, err := right.Read(data)
			if n > 0 {
				frame := binary.BigEndian.AppendUint32(nil, id)
				frame = append(frame, data[:n]...)
				writeCtx, stop := context.WithTimeout(b.ctx, 5*time.Second)
				err := b.ws.Write(writeCtx, websocket.MessageBinary, frame)
				stop()
				if err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		left.Close()
		right.Close()
		<-pumpDone
		b.mu.Lock()
		delete(b.pipes, id)
		b.mu.Unlock()
		writeCtx, stop := context.WithTimeout(b.ctx, 5*time.Second)
		defer stop()
		end, _ := json.Marshal(map[string]any{"type": "end", "conn": id})
		b.ws.Write(writeCtx, websocket.MessageText, end)
	}()
	secure := tls.Client(left, &tls.Config{RootCAs: r.pool, ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	defer secure.Close()
	secure.SetDeadline(time.Now().Add(10 * time.Second))
	if err := secure.HandshakeContext(ctx); err != nil {
		return "", err
	}
	if _, err := io.WriteString(secure, "GET / HTTP/1.1\r\nHost: "+u.Hostname()+"\r\nConnection: close\r\n\r\n"); err != nil {
		return "", err
	}
	response, err := http.ReadResponse(bufio.NewReader(secure), &http.Request{Method: "GET"})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return string(data), err
}

func TestNativeRelayFixtureKeepsSharedBridgeAlive(t *testing.T) {
	relay := newNativeRelay(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "fixture-alive") }))
	defer target.Close()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := opentunnel.New(opentunnel.Options{API: relay.server.URL, Directory: filepath.Join(t.TempDir(), "identity")})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := client.Connect(ctx, opentunnel.Routes{"fixture": func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		body, err := relay.visit(connection.URL("fixture"))
		if err != nil || body != "fixture-alive" || relay.sessions.Load() != 1 {
			t.Fatal("visitor cleanup destroyed the shared bridge", body, err, relay.sessions.Load())
		}
	}
}
