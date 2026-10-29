package sharing

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"incus-for-devs/internal/core"
)

type source struct {
	mu     sync.Mutex
	id     string
	before func()
}

var owner = core.Principal{Name: "alice", Project: "dev-alice"}

func (s *source) ServiceIdentity(_ context.Context, p core.Principal, name string) (string, error) {
	if p != owner || name != "demo" {
		return "", errors.New("instance not owned")
	}
	s.mu.Lock()
	id, before := s.id, s.before
	s.mu.Unlock()
	if before != nil {
		before()
	}
	return id, nil
}

func (s *source) DialService(context.Context, core.Principal, string, string, int) (net.Conn, error) {
	return nil, errors.New("no visitor connection expected")
}

func managerFixture(t *testing.T) (*Manager, *source, *atomic.Int32) {
	t.Helper()
	var creates atomic.Int32
	var cert string
	var certMu sync.Mutex
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/tunnel" {
			creates.Add(1)
			w.WriteHeader(201)
			ioJSON(w, map[string]any{"tunnel": map[string]string{"id": "demo", "hostname": "demo.test"}, "token": "synthetic-token"})
			return
		}
		if req.URL.Path == "/api/tunnel/demo/connect" {
			socket, err := websocket.Accept(w, req, &websocket.AcceptOptions{Subprotocols: []string{"opentunnel"}})
			if err != nil {
				return
			}
			defer socket.CloseNow()
			_, data, err := socket.Read(req.Context())
			if err != nil {
				return
			}
			var attach struct {
				Routes []string `json:"routes"`
			}
			json.Unmarshal(data, &attach)
			message, _ := json.Marshal(map[string]any{"type": "attached", "session": "session", "routes": attach.Routes, "heartbeat_ms": 1000, "idle_timeout_ms": 5000})
			socket.Write(req.Context(), websocket.MessageText, message)
			for {
				if _, _, err := socket.Read(req.Context()); err != nil {
					return
				}
			}
		}
		if req.Header.Get("Authorization") != "Bearer synthetic-token" {
			w.WriteHeader(401)
			return
		}
		certMu.Lock()
		defer certMu.Unlock()
		if req.Method == "POST" {
			var body struct {
				CSR string `json:"csr"`
			}
			json.NewDecoder(req.Body).Decode(&body)
			block, _ := pem.Decode([]byte(body.CSR))
			if block == nil {
				w.WriteHeader(400)
				return
			}
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(60 * 24 * time.Hour)}
			der, err := x509.CreateCertificate(rand.Reader, template, template, csr.PublicKey, key)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			cert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			w.WriteHeader(202)
			ioJSON(w, map[string]any{"state": map[string]string{"type": "issuing"}})
			return
		}
		ioJSON(w, map[string]any{"state": map[string]string{"type": "ready", "certificate": cert, "chain": ""}})
	}))
	t.Cleanup(server.Close)
	source := &source{id: "immutable-first-instance"}
	m := New(context.Background(), source, Options{API: server.URL, Directory: filepath.Join(t.TempDir(), "state")})
	t.Cleanup(m.Close)
	return m, source, &creates
}

func ioJSON(w http.ResponseWriter, body any) { json.NewEncoder(w).Encode(body) }

func TestExplicitPerMachinePerPortApproval(t *testing.T) {
	m, _, creates := managerFixture(t)
	ctx := context.Background()
	if shares, err := m.List(ctx, owner, ""); err != nil || len(shares) != 0 || creates.Load() != 0 {
		t.Fatal("default exposure is not private", shares, err)
	}
	for _, name := range []string{"foreign", "*", "../demo"} {
		if _, err := m.Enable(ctx, owner, name, 8080); err == nil {
			t.Fatal("accepted an unowned or wildcard machine")
		}
	}
	if creates.Load() != 0 {
		t.Fatal("invalid enable reached the public API")
	}
	first, err := m.Enable(ctx, owner, "demo", 8080)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Enable(ctx, owner, "demo", 9090)
	if err != nil {
		t.Fatal(err)
	}
	if first.URL == second.URL || creates.Load() != 1 {
		t.Fatal("ports must have separate routes but reuse the owner's identity")
	}
	if err := m.Disable(ctx, core.Principal{Name: "bob", Project: "dev-bob"}, "demo", 8080); err != nil {
		t.Fatal(err)
	}
	if shares, _ := m.List(ctx, owner, ""); len(shares) != 2 {
		t.Fatal("another user disabled an owned route")
	}
	if err := m.Disable(ctx, owner, "demo", 8080); err != nil {
		t.Fatal(err)
	}
	shares, err := m.List(ctx, owner, "demo")
	if err != nil || len(shares) != 1 || shares[0].Port != 9090 {
		t.Fatal("disabling one port affected a different port", shares, err)
	}
	m.Revoke(owner, "demo")
	if shares, _ := m.List(ctx, owner, ""); len(shares) != 0 {
		t.Fatal("machine revocation left a published port")
	}
}

func TestRecreationAndGatewayRestartRequireNewOptIn(t *testing.T) {
	m, source, _ := managerFixture(t)
	first, err := m.Enable(context.Background(), owner, "demo", 8080)
	if err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	source.id = "different-recreated-instance"
	source.mu.Unlock()
	second, err := m.Enable(context.Background(), owner, "demo", 8080)
	if err != nil || first.URL == second.URL {
		t.Fatal("explicitly re-enabling a new machine reused its old route", err)
	}
	m.Close()
	if shares, _ := m.List(context.Background(), owner, ""); len(shares) != 0 {
		t.Fatal("gateway shutdown retained exposure")
	}
}

func TestDisableInvalidatesAnEnableStillCheckingOwnership(t *testing.T) {
	m, source, creates := managerFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	source.before = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := m.Enable(context.Background(), owner, "demo", 8080); done <- err }()
	<-entered
	if err := m.Disable(context.Background(), owner, "demo", 8080); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("enable survived a completed disable")
	}
	if creates.Load() != 0 {
		t.Fatal("revoked pending enable reached the public API")
	}
}

func TestLifecycleBlocksAndInvalidatesConcurrentEnables(t *testing.T) {
	m, source, creates := managerFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	source.before = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := m.Enable(context.Background(), owner, "demo", 8080); done <- err }()
	<-entered
	if err := m.ChangeMachine(context.Background(), owner, "demo", func() error {
		if _, err := m.Enable(context.Background(), owner, "demo", 9090); err == nil {
			return errors.New("enable ran during lifecycle operation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil || creates.Load() != 0 {
		t.Fatal("pre-restart enable survived lifecycle revocation", err)
	}
}
