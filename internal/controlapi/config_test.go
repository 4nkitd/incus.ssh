package controlapi

import "testing"

func TestConfigBoundaries(t *testing.T) {
	for _, tc := range []struct {
		listen, origin, directory string
		valid                     bool
	}{
		{"127.0.0.1:8080", testOrigin, "/var/lib/incus-for-devs/api", true},
		{"[::1]:8080", testOrigin, "/var/lib/incus-for-devs/api", true},
		{"0.0.0.0:8080", testOrigin, "/private/api", false},
		{"100.64.0.1:8080", testOrigin, "/private/api", false},
		{"127.0.0.1:80", testOrigin, "/private/api", false},
		{"localhost:8080", testOrigin, "/private/api", false},
		{"127.0.0.1:8080", "http://api.example.com", "/private/api", false},
		{"127.0.0.1:8080", "https://api.example.com/path", "/private/api", false},
		{"127.0.0.1:8080", "https://user:pass@api.example.com", "/private/api", false},
		{"127.0.0.1:8080", "https://*.example.com", "/private/api", false},
		{"127.0.0.1:8080", "https://127.0.0.1", "/private/api", false},
		{"127.0.0.1:8080", testOrigin, "/", false},
		{"127.0.0.1:8080", testOrigin, "relative", false},
		{"127.0.0.1:8080", testOrigin, "/private/../api", false},
	} {
		cfg := Config{Listen: tc.listen, Origin: tc.origin, StateDirectory: tc.directory}
		if err := cfg.Validate(); (err == nil) != tc.valid {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}
