package opentunnel

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestCancelledConnectDoesNotWaitForProvisioningLock(t *testing.T) {
	r := newRelay(t)
	c := newTestClient(t, r)
	if err := c.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.Connect(ctx, Routes{"api": echoTarget(t)}); err == nil || time.Since(started) > time.Second {
		t.Fatal("cancelled connect waited for another provisioning operation", err)
	}
}

func TestExpiredIdentityAttachesToTriggerServerRenewal(t *testing.T) {
	r := newRelay(t)
	c := newTestClient(t, r)
	value, _, err := c.ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	block, _ := pem.Decode([]byte(r.csr))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(3), DNSNames: csr.DNSNames, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(-time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, r.ca, csr.PublicKey, r.key)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	r.cert = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	value.certificate = []byte(r.cert)
	r.mu.Unlock()
	if err := c.store.save(value); err != nil {
		t.Fatal(err)
	}
	connectTest(t, c, Routes{"api": echoTarget(t)})
	session := nextSession(t, r)
	pipe := session.public(1, "api.demo.test")
	defer pipe.Close()
	select {
	case reset := <-session.resets:
		if reset.Code != "unknown_route" {
			t.Fatal(reset)
		}
	case <-time.After(time.Second):
		t.Fatal("expired certificate was allowed to serve")
	}
}

func TestTransientRouteCapacityRefusalReconnects(t *testing.T) {
	r := newRelay(t)
	r.refuse = "too_many_routes"
	c := newTestClient(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := c.Connect(ctx, Routes{"api": echoTarget(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case refusal := <-r.refusals:
		if refusal != "too_many_routes" {
			t.Fatal(refusal)
		}
	case <-ctx.Done():
		t.Fatal("relay did not exercise the capacity refusal")
	}
	r.mu.Lock()
	r.refuse = ""
	r.mu.Unlock()
	if err := connection.Ready(ctx); err != nil {
		t.Fatal("temporary capacity refusal was treated as fatal", err)
	}
}

func TestAPIErrorMessagesAreUsefulButDoNotLeakToken(t *testing.T) {
	message := apiDetail(429, []byte(`{"message":"rate limited test-only-token-not-a-credential\u001b[31m"}`), testToken)
	if strings.Contains(message, testToken) || strings.Contains(message, "\x1b") || !strings.Contains(message, "rate limited") {
		t.Fatal(message)
	}
	if message := apiDetail(525, []byte("<html>failure</html>"), ""); !strings.Contains(message, "SSL handshake") {
		t.Fatal(message)
	}
}
