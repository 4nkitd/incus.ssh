package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"incus-for-devs/internal/core"
)

type hostingFake struct {
	fakeBackend
	options   core.AppOptions
	published bool
	upstream  string
	dialed    bool
}

func (f *hostingFake) HostingCapabilities(context.Context, core.Principal) (core.HostingCapabilities, error) {
	return core.HostingCapabilities{Available: true, ServerVersion: "7.0.2", Limits: core.Limits{MaxInstances: 4, CPU: 2, Memory: "2GiB"}}, nil
}
func (f *hostingFake) CreateApp(_ context.Context, p core.Principal, in core.AppOptions) (core.Instance, error) {
	f.options = in
	return core.Instance{Name: in.Name, Kind: "application", Status: "Running"}, f.record("app-create", p)
}
func (f *hostingFake) AppLogs(_ context.Context, p core.Principal, name string) (string, error) {
	return "<script>not executable</script>", f.record("logs:"+name, p)
}
func (f *hostingFake) PublishApp(_ context.Context, p core.Principal, name string, port int) error {
	f.published = port != 0
	return f.record("publish:"+name, p)
}
func (f *hostingFake) AppTarget(_ context.Context, p core.Principal, name string) (core.AppTarget, error) {
	if !f.published || name != "demo" {
		return core.AppTarget{}, errors.New("private")
	}
	return core.AppTarget{Port: 80, BootID: "verified-boot"}, nil
}
func (f *hostingFake) ServiceIdentity(context.Context, core.Principal, string) (string, error) {
	return "verified-boot", nil
}
func (f *hostingFake) DialService(ctx context.Context, p core.Principal, name, id string, port int) (net.Conn, error) {
	if p != testPrincipal || name != "demo" || id != "verified-boot" || port != 80 {
		return nil, errors.New("unsafe target")
	}
	f.dialed = true
	return (&net.Dialer{}).DialContext(ctx, "tcp", f.upstream)
}

func TestUIIsPublicButAPIRemainsProtected(t *testing.T) {
	f := newFixture(t, time.Second, 4)
	for _, path := range []string{"/ui", "/ui/", "/ui/app.js", "/ui/app.css", "/ui/IBMPlexSans-Regular.woff2"} {
		r := f.request("GET", path, "")
		r.Header.Del("Authorization")
		w := f.send(r)
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), f.token) {
			t.Fatal("credential embedded in UI")
		}
	}
	for _, path := range []string{"/api/v1/vms", "/api/v1/system", "/mcp"} {
		r := f.request("GET", path, "")
		r.Header.Del("Authorization")
		if f.send(r).Code != 401 {
			t.Fatalf("unprotected %s", path)
		}
	}
	if w := f.send(f.request("GET", "/ui/token.json", "")); w.Code != 404 {
		t.Fatal("unlisted static file served")
	}
	if w := f.send(f.request("POST", "/ui/app.js", "")); w.Code != 405 {
		t.Fatal("static endpoint accepted POST")
	}
	r := f.request("GET", "/ui", "")
	r.Host = "evil.example"
	if f.send(r).Code != 403 {
		t.Fatal("UI hostname boundary bypassed")
	}
}

func TestHostingRESTAndOriginSeparation(t *testing.T) {
	f := newFixture(t, time.Second, 4)
	backend := &hostingFake{}
	f.handler = NewHandler(f.tokens, backend, nil, time.Second, 4, "test", "apps.example.com")
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/system", "", 200},
		{"POST", "/api/v1/apps", `{"name":"demo","image":"nginx:alpine","environment":{"MODE":"production"}}`, 201},
		{"GET", "/api/v1/apps/demo/logs", "", 200},
		{"POST", "/api/v1/apps/demo/publish", `{"port":80}`, 200},
		{"POST", "/api/v1/apps/demo/publish", `{"port":0}`, 200},
		{"POST", "/api/v1/apps", `{"name":"demo","image":"nginx","privileged":true}`, 400},
	} {
		w := f.send(f.request(tc.method, tc.path, tc.body))
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if backend.options.Environment["MODE"] != "production" {
		t.Fatal("environment lost")
	}
	r := f.request("GET", "/api/v1/system", "")
	r.Header.Set("Origin", "https://demo.apps.example.com")
	if f.send(r).Code != 403 {
		t.Fatal("hosted app can access control plane")
	}
}

func TestAppProxyUsesOnlyOwnedBootAndPort(t *testing.T) {
	f := newFixture(t, time.Second, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "demo.apps.example.com" || r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Error("wrong upstream headers")
		}
		_, _ = io.WriteString(w, "application content")
	}))
	defer upstream.Close()
	backend := &hostingFake{published: true, upstream: strings.TrimPrefix(upstream.URL, "http://")}
	f.handler = NewHandler(f.tokens, backend, nil, time.Second, 4, "test", "apps.example.com")
	r := f.request("GET", "/api/v1/vms", "")
	r.Host = "demo.apps.example.com"
	r.Header.Del("Authorization")
	w := f.send(r)
	if w.Code != 200 || w.Body.String() != "application content" || !backend.dialed {
		t.Fatalf("app route: %d %s", w.Code, w.Body.String())
	}
	backend.published = false
	backend.dialed = false
	if f.send(r).Code != 503 || backend.dialed {
		t.Fatal("private app still dialed")
	}
	r.Host = "nested.demo.apps.example.com"
	if f.send(r).Code != 404 {
		t.Fatal("invalid app hostname accepted")
	}
}

func TestHostingToolsAreDiscovered(t *testing.T) {
	f := newFixture(t, time.Second, 4)
	backend := &hostingFake{}
	f.handler = NewHandler(f.tokens, backend, nil, time.Second, 4, "test", "apps.example.com")
	r := f.request("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-06-18")
	w := f.send(r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Result struct{ Tools []struct{ Name string } }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Result.Tools) != 12 {
		t.Fatal("missing hosting tools")
	}
}
