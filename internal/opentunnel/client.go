package opentunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const DefaultAPI = "https://opentunnel.xyz"

var label = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type Options struct {
	API        string
	Directory  string
	HTTPClient *http.Client
}

type Client struct {
	api   *url.URL
	http  *http.Client
	store *store
	gate  chan struct{}
}

type DialTarget func(context.Context) (net.Conn, error)

type Routes map[string]DialTarget

type AttachError struct{ Code string }

func (e *AttachError) Error() string { return "OpenTunnel attach refused: " + e.Code }

func New(options Options) (*Client, error) {
	if options.API == "" {
		options.API = DefaultAPI
	}
	u, err := url.Parse(options.API)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("OpenTunnel API must be an HTTPS origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("OpenTunnel requires HTTPS except for loopback test relays")
	}
	client := options.HTTPClient
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		client = &http.Client{Transport: transport}
	}
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	storage, err := openStore(options.Directory)
	if err != nil {
		return nil, err
	}
	return &Client{api: u, http: &copyClient, store: storage, gate: make(chan struct{}, 1)}, nil
}

func (c *Client) lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.gate <- struct{}{}:
		return nil
	}
}

func (c *Client) unlock() { <-c.gate }

func (c *Client) Close() error { return c.store.close() }

func validHostname(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if !label.MatchString(part) {
			return false
		}
	}
	return true
}

func routeForSNI(sni, hostname string) string {
	sni = strings.ToLower(sni)
	if sni == hostname {
		return "@"
	}
	if prefix, ok := strings.CutSuffix(sni, "."+hostname); ok && label.MatchString(prefix) {
		return prefix
	}
	return ""
}

func PublicURL(route, hostname string) string {
	if route == "@" {
		return "https://" + hostname
	}
	return "https://" + route + "." + hostname
}
