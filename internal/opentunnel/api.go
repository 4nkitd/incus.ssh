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
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	randv2 "math/rand/v2"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

type apiError struct {
	status    int
	transient bool
	detail    string
}

func (e *apiError) Error() string {
	if e.detail != "" {
		return "OpenTunnel API: " + e.detail
	}
	return fmt.Sprintf("OpenTunnel API returned HTTP %d", e.status)
}

func apiDetail(status int, payload []byte, token string) string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &body) == nil && body.Message != "" {
		message := body.Message
		if token != "" {
			message = strings.ReplaceAll(message, token, "[redacted]")
		}
		message = strings.Map(func(ch rune) rune {
			if unicode.IsControl(ch) {
				return ' '
			}
			return ch
		}, message)
		if len(message) > 512 {
			message = message[:512]
		}
		return fmt.Sprintf("%s (HTTP %d)", message, status)
	}
	cloudflare := map[int]string{520: "the server returned an unknown error", 521: "the server is down", 522: "the connection to the server timed out", 523: "the server is unreachable", 524: "the server timed out", 525: "the SSL handshake with the server failed", 526: "the server has an invalid SSL certificate", 527: "the connection to the server failed"}
	if meaning := cloudflare[status]; meaning != "" {
		return fmt.Sprintf("Cloudflare %d: %s (HTTP %d)", status, meaning, status)
	}
	return fmt.Sprintf("HTTP %d", status)
}

type certificateInfo struct {
	State struct {
		Type        string `json:"type"`
		Certificate string `json:"certificate"`
		Chain       string `json:"chain"`
	} `json:"state"`
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func backoff(attempt int, maximum time.Duration) time.Duration {
	base := min(250*time.Millisecond*time.Duration(1<<min(attempt, 16)), maximum)
	return base/2 + time.Duration(randv2.Int64N(int64(base/2)+1))
}

func (c *Client) request(ctx context.Context, method, path, token string, body, result any, idempotent bool) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		retry := false
		delay := backoff(attempt, 8*time.Second)
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := http.NewRequestWithContext(requestCtx, method, c.api.Scheme+"://"+c.api.Host+path, bytes.NewReader(data))
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "incus.ssh-opentunnel/1")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		var connected atomic.Bool
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}))
		response, callErr := c.http.Do(req)
		if callErr != nil {
			// Before GotConn, DNS/proxy/TLS setup has not transmitted an HTTP request to the API.
			retry = idempotent || !connected.Load()
			err = &apiError{transient: retry, detail: "connection failed"}
		} else {
			payload, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			response.Body.Close()
			status := response.StatusCode
			if status >= 200 && status <= 299 {
				if len(payload) > 1<<20 {
					cancel()
					return errors.New("invalid or oversized OpenTunnel API response")
				}
				if readErr == nil {
					cancel()
					if result != nil && json.Unmarshal(payload, result) != nil {
						return errors.New("invalid OpenTunnel API JSON")
					}
					return nil
				}
				retry = idempotent
				err = &apiError{transient: retry, detail: "response connection failed"}
			} else {
				retry = status == 429 || status == 503 || status == 521 || status == 522 || status == 523 || status == 525
				if idempotent {
					retry = status == 429 || status >= 500
				}
				if seconds, parseErr := strconv.ParseUint(response.Header.Get("Retry-After"), 10, 32); parseErr == nil {
					delay = time.Duration(min(seconds, 8)) * time.Second
				}
				err = &apiError{status: status, transient: retry, detail: apiDetail(status, payload, token)}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retry || attempt >= 6 {
			return err
		}
		if err := pause(ctx, delay); err != nil {
			return err
		}
	}
}

func certificate(value *identity) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair(append(append([]byte{}, value.certificate...), value.chain...), value.key)
	if err != nil {
		return nil, errors.New("invalid OpenTunnel certificate or private key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(value.Hostname) != nil || leaf.VerifyHostname("probe."+value.Hostname) != nil {
		return nil, errors.New("OpenTunnel certificate does not cover the tunnel hostname and wildcard")
	}
	pair.Leaf = leaf
	value.CertificateExpiry = leaf.NotAfter
	return &pair, nil
}

func (c *Client) ensure(ctx context.Context) (*identity, *tls.Certificate, error) {
	if err := c.lock(ctx); err != nil {
		return nil, nil, err
	}
	defer c.unlock()
	value, err := c.store.load()
	if err != nil {
		return nil, nil, err
	}
	if value == nil {
		var created struct {
			Tunnel struct {
				ID       string `json:"id"`
				Hostname string `json:"hostname"`
			} `json:"tunnel"`
			Token string `json:"token"`
		}
		if err := c.request(ctx, "POST", "/api/tunnel", "", struct{}{}, &created, false); err != nil {
			return nil, nil, err
		}
		if !label.MatchString(created.Tunnel.ID) || !validHostname(created.Tunnel.Hostname) || created.Token == "" || len(created.Token) > 4096 || strings.ContainsAny(created.Token, "\x00\r\n\t ") {
			return nil, nil, errors.New("invalid OpenTunnel creation response")
		}
		value = &identity{ID: created.Tunnel.ID, Hostname: created.Tunnel.Hostname, token: created.Token}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, nil, err
		}
		value.key = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
			Subject: pkix.Name{CommonName: value.Hostname}, DNSNames: []string{value.Hostname, "*." + value.Hostname}, SignatureAlgorithm: x509.ECDSAWithSHA256,
		}, key)
		if err != nil {
			return nil, nil, err
		}
		value.csr = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
		if err := c.store.savePending(value); err != nil {
			return nil, nil, err
		}
	}
	var pair *tls.Certificate
	if len(value.certificate) > 0 {
		pair, err = certificate(value)
		if err != nil {
			return nil, nil, err
		}
	} else {
		if value.csr == "" {
			return nil, nil, errors.New("incomplete pending OpenTunnel identity")
		}
		var info certificateInfo
		if err := c.request(ctx, "POST", "/api/tunnel/"+value.ID+"/certificate", value.token, map[string]string{"csr": value.csr}, &info, true); err != nil {
			var response *apiError
			if !errors.As(err, &response) || response.status != 409 {
				return nil, nil, err
			}
		}
	}
	for {
		updated, updatedPair, err := c.refresh(ctx, value)
		if err != nil {
			var response *apiError
			if pair != nil && errors.As(err, &response) && response.transient && ctx.Err() == nil {
				return value, pair, nil
			}
			return nil, nil, err
		}
		if updatedPair != nil {
			return updated, updatedPair, nil
		}
		if pair != nil {
			return value, pair, nil
		}
		if err := pause(ctx, 2*time.Second); err != nil {
			return nil, nil, err
		}
	}
}

func (c *Client) refresh(ctx context.Context, current *identity) (*identity, *tls.Certificate, error) {
	var info certificateInfo
	if err := c.request(ctx, "GET", "/api/tunnel/"+current.ID+"/certificate", current.token, nil, &info, true); err != nil {
		return nil, nil, err
	}
	if info.State.Type == "failed" {
		return nil, nil, errors.New("OpenTunnel certificate issuance failed")
	}
	if info.State.Type != "ready" {
		return current, nil, nil
	}
	updated := *current
	updated.certificate, updated.chain = []byte(info.State.Certificate), []byte(info.State.Chain)
	pair, err := certificate(&updated)
	if err != nil {
		return nil, nil, err
	}
	if !time.Now().Before(pair.Leaf.NotAfter) {
		return current, nil, nil
	}
	if err := c.store.save(&updated); err != nil {
		return nil, nil, err
	}
	return &updated, pair, nil
}
