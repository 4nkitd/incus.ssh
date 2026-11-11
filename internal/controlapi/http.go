package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"incus-for-devs/internal/core"
)

const maxBody = 64 << 10

type requestIdentity struct {
	principal core.Principal
	ctx       context.Context
}

type identityKey struct{}

func NewHandler(tokens *Tokens, backend core.Backend, sharing core.Sharing, timeout time.Duration, concurrency int, version, applicationsDomain string) http.Handler {
	s := service{backend: backend, sharing: sharing, applicationsDomain: applicationsDomain, version: version, activity: tokens.activity}
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.mcpHandler(version))
	mux.HandleFunc("/api/v1/", s.rest)
	gate := make(chan struct{}, concurrency)
	appProxy := s.appProxy(tokens.state.Principal, timeout, concurrency)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if applicationsDomain != "" && strings.HasSuffix(r.Host, "."+applicationsDomain) {
			appProxy.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != strings.TrimPrefix(tokens.origin, "https://") || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != tokens.origin) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, "host or origin is not allowed")
			return
		}
		if r.URL.RawQuery != "" {
			writeError(w, http.StatusBadRequest, "query parameters are not supported; send credentials only in Authorization")
			return
		}
		if r.URL.Path == "/ui" || strings.HasPrefix(r.URL.Path, "/ui/") {
			serveUI(w, r)
			return
		}
		authorization := r.Header.Values("Authorization")
		if len(authorization) != 1 || !strings.HasPrefix(authorization[0], "Bearer ") {
			unauthorized(w)
			return
		}
		principal, epoch, ok := tokens.authenticate(strings.TrimPrefix(authorization[0], "Bearer "))
		if !ok {
			unauthorized(w)
			return
		}
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		default:
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "too many concurrent API requests")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		stop := context.AfterFunc(epoch, cancel)
		defer stop()
		if epoch.Err() != nil {
			unauthorized(w)
			return
		}
		ctx = context.WithValue(ctx, identityKey{}, requestIdentity{principal: principal, ctx: ctx})
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="incus-for-devs"`)
	writeError(w, http.StatusUnauthorized, "valid API bearer token required")
}

func (s service) rest(w http.ResponseWriter, r *http.Request) {
	p := r.Context().Value(identityKey{}).(requestIdentity).principal
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	var result any
	var err error
	status := http.StatusOK
	switch {
	case path == "events" && r.Method == http.MethodGet:
		result = s.activity.snapshot()
	case path == "system" && r.Method == http.MethodGet:
		result, err = s.system(r.Context(), p)
	case path == "apps" && r.Method == http.MethodPost:
		var in core.AppOptions
		if err = decodeBody(r, &in); err == nil {
			result, err = s.createApp(r.Context(), p, in)
			status = http.StatusCreated
		}
	case strings.HasPrefix(path, "apps/"):
		parts := strings.Split(path, "/")
		if len(parts) != 3 {
			writeError(w, 404, "unknown app endpoint")
			return
		}
		if parts[2] == "details" && r.Method == http.MethodGet {
			result, err = s.inspectApp(r.Context(), p, parts[1])
		} else if parts[2] == "environment" && r.Method == http.MethodPatch {
			var in core.EnvironmentUpdate
			if err = decodeBody(r, &in); err == nil {
				result, err = s.updateEnvironment(r.Context(), p, parts[1], in)
			}
		} else if parts[2] == "logs" && r.Method == http.MethodGet {
			result, err = s.appLogs(r.Context(), p, parts[1])
		} else if parts[2] == "publish" && r.Method == http.MethodPost {
			var in struct {
				Port int `json:"port"`
			}
			if err = decodeBody(r, &in); err == nil {
				result, err = s.publishApp(r.Context(), p, parts[1], in.Port)
			}
		} else {
			writeError(w, 404, "unknown app endpoint")
			return
		}
	case path == "vms" && r.Method == http.MethodGet:
		result, err = s.list(r.Context(), p)
	case path == "images" && r.Method == http.MethodGet:
		result, err = s.images(r.Context(), p)
	case path == "vms" && r.Method == http.MethodPost:
		var in CreateInput
		if err = decodeBody(r, &in); err == nil {
			result, err = s.create(r.Context(), p, in)
			status = http.StatusCreated
		}
	default:
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] == "vms" && r.Method == http.MethodDelete {
			result, err = s.lifecycle(r.Context(), p, "delete", parts[1])
		} else if len(parts) == 3 && parts[0] == "vms" && r.Method == http.MethodPost {
			if parts[2] == "exec" {
				var body struct {
					Command string `json:"command"`
					Stdin   string `json:"stdin,omitempty"`
				}
				if err = decodeBody(r, &body); err == nil {
					result, err = s.exec(r.Context(), p, ExecInput{Name: parts[1], Command: body.Command, Stdin: body.Stdin})
				}
			} else if parts[2] == "start" || parts[2] == "stop" || parts[2] == "restart" {
				result, err = s.lifecycle(r.Context(), p, parts[2], parts[1])
			} else {
				writeError(w, http.StatusNotFound, "unknown lifecycle endpoint")
				return
			}
		} else {
			writeError(w, http.StatusNotFound, "unknown endpoint or method")
			return
		}
	}
	if err != nil {
		status = http.StatusBadGateway
		var input inputError
		var tooLarge *http.MaxBytesError
		var userError core.UserError
		switch {
		case errors.As(err, &tooLarge):
			status = http.StatusRequestEntityTooLarge
		case errors.As(err, &input):
			status = http.StatusBadRequest
		case errors.As(err, &userError):
			status = http.StatusBadRequest
		case errors.Is(err, context.DeadlineExceeded):
			status = http.StatusGatewayTimeout
		case errors.Is(err, context.Canceled):
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, status, result)
}

func decodeBody(r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return badInput("Content-Type must be application/json")
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return badInput("body must be a JSON object")
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return badInput("invalid JSON or unknown field")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return badInput("body must contain exactly one JSON object")
	}
	return nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
