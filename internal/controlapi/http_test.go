package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"incus-for-devs/internal/core"
)

type fakeBackend struct {
	mu         sync.Mutex
	calls      []string
	principals []core.Principal
	opts       core.CreateOptions
	execFn     func(context.Context, core.Streams) (int, error)
	err        error
}

func (f *fakeBackend) record(action string, p core.Principal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action)
	f.principals = append(f.principals, p)
	return f.err
}
func (f *fakeBackend) List(ctx context.Context, p core.Principal) ([]core.Instance, error) {
	return []core.Instance{{Name: "demo", Status: "Running"}}, f.record("list", p)
}
func (f *fakeBackend) Images(ctx context.Context, p core.Principal) ([]core.ImageInfo, error) {
	return nil, f.record("images", p)
}
func (f *fakeBackend) Create(ctx context.Context, p core.Principal, o core.CreateOptions) (core.Instance, error) {
	f.opts = o
	return core.Instance{Name: o.Name, Status: "Running"}, f.record("create", p)
}
func (f *fakeBackend) Remove(ctx context.Context, p core.Principal, n string) error {
	return f.record("delete:"+n, p)
}
func (f *fakeBackend) Start(ctx context.Context, p core.Principal, n string) error {
	return f.record("start:"+n, p)
}
func (f *fakeBackend) Resume(ctx context.Context, p core.Principal, n string) error {
	return f.Start(ctx, p, n)
}
func (f *fakeBackend) Stop(ctx context.Context, p core.Principal, n string) error {
	return f.record("stop:"+n, p)
}
func (f *fakeBackend) Restart(ctx context.Context, p core.Principal, n string) error {
	return f.record("restart:"+n, p)
}
func (f *fakeBackend) Exec(ctx context.Context, p core.Principal, n, cmd string, s core.Streams) (int, error) {
	if err := f.record("exec:"+n+":"+cmd, p); err != nil {
		return 0, err
	}
	if f.execFn != nil {
		return f.execFn(ctx, s)
	}
	_, _ = io.Copy(s.Out, s.In)
	_, _ = io.WriteString(s.Err, "example stderr")
	return 7, nil
}

type fixture struct {
	tokens  *Tokens
	token   string
	backend *fakeBackend
	handler http.Handler
}

func newFixture(t *testing.T, timeout time.Duration, concurrency int) fixture {
	t.Helper()
	tokens, err := OpenTokens(filepath.Join(t.TempDir(), "api"), testOrigin, testPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tokens.Close)
	status, err := tokens.Manage(testPrincipal, "enable")
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	return fixture{tokens, status.Token, backend, NewHandler(tokens, backend, nil, timeout, concurrency, "test", "")}
}

func (f fixture) request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, testOrigin+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func (f fixture) send(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestRESTLifecycleAndExecution(t *testing.T) {
	f := newFixture(t, time.Second, 2)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/vms", "", 200},
		{"GET", "/api/v1/images", "", 200},
		{"POST", "/api/v1/vms", `{"name":"demo","cpu":2,"memory":"2GiB"}`, 201},
		{"POST", "/api/v1/vms/demo/start", "", 200},
		{"POST", "/api/v1/vms/demo/stop", "", 200},
		{"POST", "/api/v1/vms/demo/restart", "", 200},
		{"POST", "/api/v1/vms/demo/exec", `{"command":"cat","stdin":"hello"}`, 200},
		{"DELETE", "/api/v1/vms/demo", "", 200},
	} {
		w := f.send(f.request(tc.method, tc.path, tc.body))
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if strings.HasSuffix(tc.path, "/exec") {
			var out map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out["stdout"] != "hello" || out["exit_code"] != float64(7) {
				t.Fatal(out)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("response may be cached")
		}
	}
	if f.backend.opts.CPU != 2 || f.backend.opts.Image != "" {
		t.Fatal("create options not preserved")
	}
	for _, p := range f.backend.principals {
		if p != testPrincipal {
			t.Fatal("principal boundary changed")
		}
	}
}

func TestHTTPAuthenticationAndBrowserBoundaries(t *testing.T) {
	f := newFixture(t, time.Second, 2)
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"missing", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"wrong", func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }, 401},
		{"cookie", func(r *http.Request) { r.Header.Del("Authorization"); r.Header.Set("Cookie", "token="+f.token) }, 401},
		{"duplicate", func(r *http.Request) { r.Header.Add("Authorization", "Bearer "+f.token) }, 401},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"host", func(r *http.Request) { r.Host = "evil.example" }, 403},
		{"cross-site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"query-token", func(r *http.Request) { r.URL.RawQuery = "token=" + f.token }, 400},
	} {
		for _, path := range []string{"/api/v1/vms", "/mcp"} {
			t.Run(tc.name+path, func(t *testing.T) {
				r := f.request("POST", path, `{}`)
				tc.change(r)
				w := f.send(r)
				if w.Code != tc.status {
					t.Fatalf("got %d", w.Code)
				}
				if strings.Contains(w.Body.String(), f.token) {
					t.Fatal("token reflected")
				}
			})
		}
	}
	if len(f.backend.calls) != 0 {
		t.Fatal("unauthenticated call reached backend")
	}
	_, _ = f.tokens.Manage(testPrincipal, "disable")
	if w := f.send(f.request("GET", "/api/v1/vms", "")); w.Code != 401 {
		t.Fatal("disabled token accepted")
	}
}

func TestRESTInputValidationAndErrorRedaction(t *testing.T) {
	f := newFixture(t, time.Second, 2)
	for _, body := range []string{`null`, `[]`, `{"project":"default"}`, `{} {}`, `{"name":"--help"}`, `{"image":"images:ubuntu/24.04"}`, `{"cpu":-1}`, `{"memory":"0GiB"}`} {
		if w := f.send(f.request("POST", "/api/v1/vms", body)); w.Code != 400 {
			t.Fatalf("%s returned %d", body, w.Code)
		}
	}
	for _, body := range []string{`{}`, `{"command":""}`, `{"command":"a\u0000b"}`, `{"command":"ls","project":"default"}`} {
		if w := f.send(f.request("POST", "/api/v1/vms/demo/exec", body)); w.Code != 400 {
			t.Fatalf("%s returned %d", body, w.Code)
		}
	}
	if len(f.backend.calls) != 0 {
		t.Fatal("invalid input reached backend")
	}
	w := f.send(f.request("POST", "/api/v1/vms", `{"name":"`+strings.Repeat("x", maxBody)+`"}`))
	if w.Code != 413 {
		t.Fatalf("oversized body returned %d", w.Code)
	}
	f.backend.err = errors.New("secret internal stderr at /root/private")
	w = f.send(f.request("GET", "/api/v1/vms", ""))
	if w.Code != 502 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("backend diagnostics exposed")
	}
}

func TestRequestTimeoutConcurrencyAndRevocation(t *testing.T) {
	for _, transport := range []string{"rest", "mcp-legacy", "mcp-current"} {
		for _, action := range []string{"timeout", "disable", "rotate"} {
			t.Run(transport+"/"+action, func(t *testing.T) {
				timeout := time.Second
				if action == "timeout" {
					timeout = 50 * time.Millisecond
				}
				f := newFixture(t, timeout, 1)
				started := make(chan struct{})
				f.backend.execFn = func(ctx context.Context, _ core.Streams) (int, error) {
					close(started)
					<-ctx.Done()
					return -1, ctx.Err()
				}
				r := f.request("POST", "/api/v1/vms/demo/exec", `{"command":"sleep 60"}`)
				if transport != "rest" {
					r = f.request("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec_vm","arguments":{"name":"demo","command":"sleep 60"}}}`)
					r.Header.Set("Accept", "application/json, text/event-stream")
					version := "2025-06-18"
					if transport == "mcp-current" {
						version = "2026-07-28"
						r.Header.Set("Mcp-Method", "tools/call")
						r.Header.Set("Mcp-Name", "exec_vm")
						r.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}},"name":"exec_vm","arguments":{"name":"demo","command":"sleep 60"}}}`))
					}
					r.Header.Set("MCP-Protocol-Version", version)
				}
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- f.send(r) }()
				select {
				case <-started:
				case w := <-done:
					t.Fatalf("exec did not start: %d %s", w.Code, w.Body.String())
				case <-time.After(2 * time.Second):
					t.Fatal("exec not started")
				}
				if w := f.send(f.request("GET", "/api/v1/vms", "")); w.Code != 429 {
					t.Fatalf("concurrency limit returned %d", w.Code)
				}
				if action != "timeout" {
					if _, err := f.tokens.Manage(testPrincipal, action); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("request not canceled")
				}
			})
		}
	}
}

func TestBoundedExecOutput(t *testing.T) {
	f := newFixture(t, time.Second, 2)
	f.backend.execFn = func(ctx context.Context, s core.Streams) (int, error) {
		_, _ = s.Out.Write(bytes.Repeat([]byte("x"), 2<<20))
		return 0, nil
	}
	w := f.send(f.request("POST", "/api/v1/vms/demo/exec", `{"command":"noisy"}`))
	var result struct {
		Stdout    string
		Truncated bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Stdout) != 1<<20 || !result.Truncated {
		t.Fatal("output was not bounded")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMCPClientEndToEnd(t *testing.T) {
	f := newFixture(t, 3*time.Second, 4)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Host = "api.example.com"
		r.Header.Set("Authorization", "Bearer "+f.token)
		return http.DefaultTransport.RoundTrip(r)
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 8 {
		t.Fatalf("discovery: %v, %#v", err, tools)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "exec_vm" && (tool.Annotations == nil || tool.Annotations.ReadOnlyHint) {
			t.Fatal("exec marked read-only")
		}
	}
	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"list_vms", map[string]any{}},
		{"list_images", map[string]any{}},
		{"create_vm", map[string]any{"name": "demo"}},
		{"start_vm", map[string]any{"name": "demo"}},
		{"stop_vm", map[string]any{"name": "demo"}},
		{"restart_vm", map[string]any{"name": "demo"}},
		{"exec_vm", map[string]any{"name": "demo", "command": "cat", "stdin": "hello"}},
		{"delete_vm", map[string]any{"name": "demo"}},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %#v", call.name, err, result)
		}
	}
	before := len(f.backend.calls)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "exec_vm", Arguments: map[string]any{"name": "demo", "command": "id", "project": "default"}})
	if err == nil && !result.IsError {
		t.Fatal("unknown MCP input field accepted")
	}
	if len(f.backend.calls) != before {
		t.Fatal("invalid MCP call reached backend")
	}
	_, err = f.tokens.Manage(testPrincipal, "rotate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ListTools(ctx, nil); err == nil {
		t.Fatal("existing MCP client bypassed token rotation")
	}
}
