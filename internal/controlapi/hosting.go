package controlapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
)

func (s service) system(ctx context.Context, p core.Principal) (any, error) {
	caps := core.HostingCapabilities{Reason: "This backend does not support app hosting."}
	if backend, ok := s.backend.(core.HostingBackend); ok {
		var err error
		caps, err = backend.HostingCapabilities(ctx, p)
		if err != nil {
			return nil, operationError(ctx, err)
		}
	}
	_, inspection := s.backend.(core.AppInspector)
	return map[string]any{"version": s.version, "principal": p, "hosting": caps, "applications_domain": s.applicationsDomain, "inspection": inspection}, nil
}

func (s service) createApp(ctx context.Context, p core.Principal, in core.AppOptions) (any, error) {
	backend, ok := s.backend.(core.HostingBackend)
	if !ok {
		return nil, badInput("App hosting is not available on this backend.")
	}
	app, err := backend.CreateApp(ctx, p, in)
	s.activity.record("deploy_app", in.Name, err == nil)
	return map[string]any{"vm": app}, operationError(ctx, err)
}

func (s service) appLogs(ctx context.Context, p core.Principal, name string) (any, error) {
	if err := command.ValidateName(name); err != nil {
		return nil, inputError{err}
	}
	backend, ok := s.backend.(core.HostingBackend)
	if !ok {
		return nil, badInput("Logs are not available on this backend.")
	}
	text, err := backend.AppLogs(ctx, p, name)
	return map[string]any{"text": text}, operationError(ctx, err)
}

func (s service) publishApp(ctx context.Context, p core.Principal, name string, port int) (any, error) {
	if err := command.ValidateName(name); err != nil {
		return nil, inputError{err}
	}
	if s.applicationsDomain == "" && port != 0 {
		return nil, badInput("Ask the operator to configure an applications domain and its Caddy route first.")
	}
	backend, ok := s.backend.(core.HostingBackend)
	if !ok {
		return nil, badInput("App hosting is not available on this backend.")
	}
	err := backend.PublishApp(ctx, p, name, port)
	action := "publish"
	if port == 0 {
		action = "unpublish"
	}
	s.activity.record(action, name, err == nil)
	url := ""
	if port != 0 {
		url = "https://" + name + "." + s.applicationsDomain
	}
	return map[string]any{"name": name, "url": url}, operationError(ctx, err)
}

func (s service) appProxy(principal core.Principal, timeout time.Duration, concurrency int) http.Handler {
	gate := make(chan struct{}, concurrency)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend, ok := s.backend.(core.HostingBackend)
		services, canDial := s.backend.(core.ServiceBackend)
		name := strings.TrimSuffix(r.Host, "."+s.applicationsDomain)
		if !ok || !canDial || s.applicationsDomain == "" || command.ValidateName(name) != nil {
			http.NotFound(w, r)
			return
		}
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		default:
			http.Error(w, "App is busy. Try again shortly.", 429)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		target, err := backend.AppTarget(ctx, principal, name)
		if err != nil {
			http.Error(w, "App is private, stopped, or requires publishing again after a restart.", 503)
			return
		}
		transport := &http.Transport{
			DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return services.DialService(ctx, principal, name, target.BootID, target.Port)
			},
		}
		defer transport.CloseIdleConnections()
		proxy := &httputil.ReverseProxy{
			Rewrite: func(p *httputil.ProxyRequest) {
				p.Out.URL.Scheme = "http"
				p.Out.URL.Host = "owned-app.invalid"
				p.Out.Host = r.Host
				p.Out.Header.Del("Forwarded")
				p.Out.Header.Del("X-Forwarded-Host")
				p.Out.Header.Del("X-Forwarded-Proto")
				p.Out.Header.Del("X-Forwarded-For")
				p.Out.Header.Set("X-Forwarded-Host", r.Host)
				p.Out.Header.Set("X-Forwarded-Proto", "https")
			},
			Transport: transport,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, "The app is not responding on its configured HTTP port.", 502)
			},
			ModifyResponse: func(response *http.Response) error {
				if response.StatusCode == http.StatusSwitchingProtocols {
					return errors.New("WebSocket upgrades are not supported in this version")
				}
				return nil
			},
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}
