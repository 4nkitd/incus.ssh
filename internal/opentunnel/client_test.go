package opentunnel

import (
	"bytes"
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
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testToken = "test-only-token-not-a-credential"

type testRelay struct {
	t          *testing.T
	server     *httptest.Server
	ca         *x509.Certificate
	key        *ecdsa.PrivateKey
	pool       *x509.CertPool
	mu         sync.Mutex
	cert       string
	csr        string
	refuse     string
	creates    int
	gets       int
	postStatus int
	getStatus  int
	silent     bool
	sessions   chan *testSession
	refusals   chan string
}

type testSession struct {
	relay    *testRelay
	socket   *websocket.Conn
	mu       sync.Mutex
	pipes    map[uint32]net.Conn
	resets   chan control
	controls chan control
	routes   []string
}

func newRelay(t *testing.T) *testRelay {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRelay{t: t, ca: ca, key: key, pool: x509.NewCertPool(), sessions: make(chan *testSession, 20), refusals: make(chan string, 32)}
	r.pool.AddCert(ca)
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

func (r *testRelay) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/api/tunnel/demo/connect" {
		r.bridge(w, req)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if req.URL.Path == "/api/tunnel" && req.Method == "POST" {
		r.creates++
		if r.postStatus != 0 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(r.postStatus)
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"tunnel": map[string]string{"id": "demo", "hostname": "demo.test"}, "token": testToken})
		return
	}
	if req.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(401)
		return
	}
	if req.URL.Path != "/api/tunnel/demo/certificate" {
		w.WriteHeader(404)
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
		if err != nil || csr.CheckSignature() != nil || csr.Subject.CommonName != "demo.test" || len(csr.DNSNames) != 2 || csr.DNSNames[0] != "demo.test" || csr.DNSNames[1] != "*.demo.test" || csr.SignatureAlgorithm != x509.ECDSAWithSHA256 {
			w.WriteHeader(400)
			return
		}
		r.csr = body.CSR
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
	r.gets++
	if r.getStatus != 0 {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(r.getStatus)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"state": map[string]string{"type": "ready", "certificate": r.cert, "chain": "", "expiry": time.Now().Add(60 * 24 * time.Hour).Format(time.RFC3339)}})
}

func (r *testRelay) bridge(w http.ResponseWriter, req *http.Request) {
	socket, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{"opentunnel"}})
	if err != nil {
		return
	}
	defer socket.CloseNow()
	socket.SetReadLimit(64 * 1024)
	ctx := req.Context()
	_, data, err := socket.Read(ctx)
	if err != nil {
		return
	}
	var attach struct {
		Type      string   `json:"type"`
		Token     string   `json:"token"`
		Transport string   `json:"transport"`
		Routes    []string `json:"routes"`
		Client    struct {
			MaxConns int `json:"max_conns"`
		} `json:"client"`
	}
	if json.Unmarshal(data, &attach) != nil || attach.Type != "attach" || attach.Transport != "ws" || attach.Client.MaxConns != maxConnections {
		return
	}
	r.mu.Lock()
	refuse, silent := r.refuse, r.silent
	r.mu.Unlock()
	if attach.Token != testToken {
		refuse = "bad_token"
	}
	if refuse != "" {
		socket.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":"attach_error","code":%q}`, refuse)))
		r.refusals <- refuse
		return
	}
	if silent {
		socket.Read(ctx)
		return
	}
	s := &testSession{relay: r, socket: socket, pipes: make(map[uint32]net.Conn), resets: make(chan control, 64), controls: make(chan control, 64), routes: attach.Routes}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, pipe := range s.pipes {
			pipe.Close()
		}
	}()
	s.send(map[string]any{"type": "attached", "session": "test-session", "routes": attach.Routes, "heartbeat_ms": 1000, "idle_timeout_ms": 5000})
	r.sessions <- s
	for {
		kind, data, err := socket.Read(ctx)
		if err != nil {
			return
		}
		if kind == websocket.MessageBinary {
			if len(data) < 4 || len(data) > 32772 {
				r.t.Error("client emitted an invalid data frame")
				return
			}
			s.mu.Lock()
			pipe := s.pipes[binary.BigEndian.Uint32(data)]
			s.mu.Unlock()
			if pipe != nil {
				pipe.SetWriteDeadline(time.Now().Add(3 * time.Second))
				pipe.Write(data[4:])
			}
			continue
		}
		var message control
		if json.Unmarshal(data, &message) != nil {
			return
		}
		switch message.Type {
		case "reset":
			s.resets <- message
			s.mu.Lock()
			if pipe := s.pipes[message.Conn]; pipe != nil {
				pipe.Close()
			}
			s.mu.Unlock()
		case "ping":
			s.send(map[string]any{"type": "pong", "time_sent": message.TimeSent})
		case "pong":
			s.controls <- message
		}
	}
}

func (s *testSession) send(message any) error {
	data, _ := json.Marshal(message)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.socket.Write(ctx, websocket.MessageText, data)
}

func (s *testSession) public(id uint32, sni string) net.Conn {
	left, right := net.Pipe()
	s.mu.Lock()
	s.pipes[id] = right
	s.mu.Unlock()
	s.send(map[string]any{"type": "open", "conn": id, "sni": sni, "peer": "192.0.2.5:54321", "alpn": ""})
	go func() {
		defer right.Close()
		buffer := make([]byte, 32768)
		for {
			n, err := right.Read(buffer)
			if n > 0 {
				frame := make([]byte, n+4)
				binary.BigEndian.PutUint32(frame, id)
				copy(frame[4:], buffer[:n])
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				writeErr := s.socket.Write(ctx, websocket.MessageBinary, frame)
				cancel()
				if writeErr != nil {
					return
				}
			}
			if err != nil {
				s.send(map[string]any{"type": "end", "conn": id})
				return
			}
		}
	}()
	return left
}

func newTestClient(t *testing.T, relay *testRelay) *Client {
	t.Helper()
	c, err := New(Options{API: relay.server.URL, Directory: filepath.Join(t.TempDir(), "identity")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func connectTest(t *testing.T, c *Client, routes Routes) *Connection {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	connection, err := c.Connect(ctx, routes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connection.Close)
	if err := connection.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return connection
}

func nextSession(t *testing.T, r *testRelay) *testSession {
	t.Helper()
	select {
	case session := <-r.sessions:
		return session
	case <-time.After(5 * time.Second):
		t.Fatal("no bridge session")
		return nil
	}
}

func echoTarget(t *testing.T) DialTarget {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	return func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
}

func TestTLSForwardingAndExplicitRouteIsolation(t *testing.T) {
	r := newRelay(t)
	c := newTestClient(t, r)
	var dials atomic.Int32
	target := echoTarget(t)
	connection := connectTest(t, c, Routes{"enabled-port": func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		return target(ctx)
	}})
	if got := connection.URL("enabled-port"); got != "https://enabled-port.demo.test" {
		t.Fatal(got)
	}
	session := nextSession(t, r)
	public := session.public(1, "enabled-port.demo.test")
	secure := tls.Client(public, &tls.Config{ServerName: "enabled-port.demo.test", RootCAs: r.pool, MinVersion: tls.VersionTLS12})
	defer secure.Close()
	secure.SetDeadline(time.Now().Add(8 * time.Second))
	if err := secure.Handshake(); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("binary\x00\xffpayload"), 14000)
	written := make(chan error, 1)
	go func() { _, err := secure.Write(payload); written <- err }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(secure, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("TLS round trip: %v, matching=%v", err, bytes.Equal(got, payload))
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	for i, sni := range []string{"disabled-port.demo.test", "deep.enabled-port.demo.test", "unrelated.test", "demo.test", ""} {
		pipe := session.public(uint32(i+10), sni)
		pipe.Close()
		select {
		case reset := <-session.resets:
			if reset.Code != "unknown_route" || reset.Conn != uint32(i+10) {
				t.Fatalf("unexpected reset: %+v", reset)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("disabled route was not rejected")
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("unapproved route dialed a target: %d", dials.Load())
	}
}

func TestInnerSNIIsCheckedBeforeTargetDial(t *testing.T) {
	r := newRelay(t)
	c := newTestClient(t, r)
	var dials atomic.Int32
	connectTest(t, c, Routes{"api": func(context.Context) (net.Conn, error) { dials.Add(1); return nil, errors.New("must not dial") }})
	session := nextSession(t, r)
	public := session.public(1, "api.demo.test")
	secure := tls.Client(public, &tls.Config{ServerName: "other.demo.test", RootCAs: r.pool})
	defer secure.Close()
	secure.SetDeadline(time.Now().Add(3 * time.Second))
	if err := secure.Handshake(); err == nil {
		t.Fatal("accepted a different inner TLS SNI")
	}
	if dials.Load() != 0 {
		t.Fatal("target dial happened before route validation")
	}
}

func TestNoProvisioningWithoutExplicitRoutes(t *testing.T) {
	r := newRelay(t)
	c := newTestClient(t, r)
	for _, routes := range []Routes{nil, {}, {"*": echoTarget(t)}, {"deep.name": echoTarget(t)}, {"ok": nil}} {
		if _, err := c.Connect(context.Background(), routes); err == nil {
			t.Fatal("accepted invalid or implicit routes")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.creates != 0 {
		t.Fatal("provisioned without explicit routes")
	}
}

func TestPersistentIdentityAndPrivateFiles(t *testing.T) {
	r := newRelay(t)
	c := newTestClient(t, r)
	connection := connectTest(t, c, Routes{"api": echoTarget(t)})
	connection.Close()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"token", "private-key.pem", "certificate.pem", "chain.pem", "tunnel.json"} {
		info, err := os.Stat(filepath.Join(c.store.dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe %s: %v", name, err)
		}
	}
	c2, err := New(Options{API: r.server.URL, Directory: c.store.dir})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	second := connectTest(t, c2, Routes{"second-port": echoTarget(t)})
	second.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.creates != 1 {
		t.Fatalf("recreated existing identity %d times", r.creates)
	}
}

func TestAttachErrorsAreFatal(t *testing.T) {
	for _, code := range []string{"bad_token", "cert_not_ready", "route_conflict"} {
		t.Run(code, func(t *testing.T) {
			r := newRelay(t)
			r.refuse = code
			c := newTestClient(t, r)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			connection, err := c.Connect(ctx, Routes{"api": echoTarget(t)})
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			var attach *AttachError
			if err := connection.Ready(ctx); !errors.As(err, &attach) || attach.Code != code {
				t.Fatalf("attach error = %v", err)
			}
		})
	}
}

func TestReconnectAndHeartbeat(t *testing.T) {
	r := newRelay(t)
	c := newTestClient(t, r)
	connection := connectTest(t, c, Routes{"api": echoTarget(t)})
	first := nextSession(t, r)
	first.send(map[string]any{"type": "ping", "time_sent": 12345})
	select {
	case message := <-first.controls:
		if message.Type != "pong" || message.TimeSent != 12345 {
			t.Fatalf("heartbeat response: %+v", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat response")
	}
	first.socket.CloseNow()
	second := nextSession(t, r)
	if len(second.routes) != 1 || second.routes[0] != "api" {
		t.Fatal("reconnect changed enabled routes")
	}
	connection.Close()
	if state, _ := connection.Status(); state != "stopped" {
		t.Fatal(state)
	}
}

func TestCreateRetrySafety(t *testing.T) {
	for _, status := range []int{429, 503, 521, 522, 523, 525, 500, 502, 520, 524, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			r := newRelay(t)
			r.postStatus = status
			c := newTestClient(t, r)
			_, err := c.Connect(context.Background(), Routes{"api": echoTarget(t)})
			if err == nil {
				t.Fatal("unexpected provisioning success")
			}
			expected := 1
			if status == 429 || status == 503 || status == 521 || status == 522 || status == 523 || status == 525 {
				expected = 7
			}
			if r.creates != expected {
				t.Fatalf("HTTP %d: created %d times, expected %d", status, r.creates, expected)
			}
		})
	}
}

func TestPendingProvisionCanResume(t *testing.T) {
	r := newRelay(t)
	r.getStatus = 503
	c := newTestClient(t, r)
	if _, err := c.Connect(context.Background(), Routes{"api": echoTarget(t)}); err == nil {
		t.Fatal("expected certificate failure")
	}
	if _, err := os.Stat(filepath.Join(c.store.dir, "pending.json")); err != nil {
		t.Fatal("pending identity not saved", err)
	}
	r.mu.Lock()
	r.getStatus = 0
	r.mu.Unlock()
	connectTest(t, c, Routes{"api": echoTarget(t)})
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.creates != 1 {
		t.Fatal("resume created another tunnel")
	}
}

func TestRejectUnsafeStorageAndPlaintextAPI(t *testing.T) {
	for _, api := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com?token=x", "file:///identity", "https://example.com/#fragment"} {
		if _, err := New(Options{API: api, Directory: filepath.Join(t.TempDir(), "identity")}); err == nil {
			t.Fatalf("accepted unsafe API %s", api)
		}
	}
	dir := t.TempDir()
	os.Chmod(dir, 0755)
	if _, err := New(Options{Directory: dir}); err == nil {
		t.Fatal("accepted a non-private directory")
	}
	r := newRelay(t)
	c := newTestClient(t, r)
	if _, err := New(Options{Directory: c.store.dir}); err == nil {
		t.Fatal("accepted a second writer")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("not an identity"), 0600)
	if err := os.Symlink(outside, filepath.Join(c.store.dir, "tunnel.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.load(); err == nil {
		t.Fatal("followed a state-file symlink")
	}
}

func TestCancellationReleasesSilentBridge(t *testing.T) {
	r := newRelay(t)
	r.silent = true
	c := newTestClient(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	connection, err := c.Connect(ctx, Routes{"api": echoTarget(t)})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-connection.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled bridge did not close")
	}
	if err := connection.Ready(context.Background()); !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "bridge") {
		t.Fatal(err)
	}
}
