package app

import (
	"strings"
	"testing"

	"incus-for-devs/internal/controlapi"
)

func TestAPISetupPlanAndDomainValidation(t *testing.T) {
	if err := setupAPI([]string{"--domain", "api.example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []string{"", "https://api.example.com", "*.example.com", "api.example.com\n{", "localhost", "127.0.0.1", "api.example.com:443", "-api.example.com", "api..example.com"} {
		if err := setupAPI([]string{"--domain", domain}); err == nil {
			t.Fatalf("unsafe domain accepted: %q", domain)
		}
	}
	if err := setupAPI([]string{"--domain", "api.example.com", "extra"}); err == nil {
		t.Fatal("extra arguments accepted")
	}
}

func TestCaddyConfigurationBoundaries(t *testing.T) {
	text := caddyfile("api.example.com")
	for _, want := range []string{"admin off", "api.example.com {", "@management path /api/v1/* /mcp /ui /ui/*", "reverse_proxy 127.0.0.1:8080", `respond "Not found" 404`} {
		if !strings.Contains(text, want) {
			t.Fatalf("Caddy config missing %q", want)
		}
	}
	if strings.Contains(text, "tls internal") || strings.Contains(text, "log {") {
		t.Fatal("unexpected TLS or access logging configuration")
	}
}

func TestAPIConfigOptInAndListenerConflict(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.API != nil {
		t.Fatal("API unexpectedly enabled by default")
	}
	cfg.API = &controlapi.Config{Listen: cfg.Listen, Origin: "https://api.example.com", StateDirectory: "/var/lib/incus-for-devs/api"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("API allowed to share SSH listener")
	}
	cfg.API.Listen = "127.0.0.1:8080"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
