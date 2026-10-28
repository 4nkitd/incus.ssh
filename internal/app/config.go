package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"incus-for-devs/internal/core"
)

type User struct {
	Name           string   `json:"name"`
	Project        string   `json:"project"`
	AuthorizedKeys []string `json:"authorized_keys"`
}

type Config struct {
	Listen                  string      `json:"listen"`
	HostKey                 string      `json:"host_key"`
	IncusBinary             string      `json:"incus_binary"`
	Image                   string      `json:"image"`
	Profile                 string      `json:"profile"`
	OperationTimeoutSeconds int         `json:"operation_timeout_seconds"`
	MaxSessions             int         `json:"max_sessions"`
	Limits                  core.Limits `json:"limits"`
	Users                   []User      `json:"users"`
}

var slug = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,46}[a-z0-9])?$`)

func LoadConfig(path string) (Config, error) {
	var cfg Config
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return cfg, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return cfg, errors.New("configuration must be a regular file not writable by group or others")
	}
	if info.Size() > 1<<20 {
		return cfg, errors.New("configuration exceeds 1 MiB")
	}
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("decode configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("configuration must contain exactly one JSON object")
	}
	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:2222"
	}
	if err := validateListen(c.Listen); err != nil {
		return err
	}
	if !filepath.IsAbs(c.HostKey) || !filepath.IsAbs(c.IncusBinary) {
		return errors.New("host_key and incus_binary must be absolute paths")
	}
	if c.Image == "" || !slug.MatchString(c.Profile) {
		return errors.New("image and a valid fixed profile are required")
	}
	if c.OperationTimeoutSeconds == 0 {
		c.OperationTimeoutSeconds = 120
	}
	if c.OperationTimeoutSeconds < 1 || c.OperationTimeoutSeconds > 600 {
		return errors.New("operation_timeout_seconds must be between 1 and 600")
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = 8
	}
	if c.MaxSessions < 1 || c.MaxSessions > 64 {
		return errors.New("max_sessions must be between 1 and 64")
	}
	if c.Limits.MaxInstances < 1 || c.Limits.CPU < 1 || c.Limits.Memory == "" {
		return errors.New("positive instance/CPU limits and a memory limit are required")
	}
	if c.Limits.Processes < 0 {
		return errors.New("process limit must not be negative")
	}
	if c.Limits.Processes == 0 {
		c.Limits.Processes = 256
	}
	// Admission is per-project until a global resource allocator is implemented.
	if len(c.Users) != 1 {
		return errors.New("this pilot requires exactly one configured user")
	}
	for _, u := range c.Users {
		if !slug.MatchString(u.Name) || !slug.MatchString(u.Project) || u.Project == "default" {
			return errors.New("user and non-default project must be lowercase names")
		}
		if len(u.AuthorizedKeys) == 0 || len(u.AuthorizedKeys) > 16 {
			return errors.New("each user requires between 1 and 16 authorized public keys")
		}
	}
	return nil
}

func validateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen must be an IP:port: %w", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1024 || p > 65535 {
		return errors.New("listen port must be between 1024 and 65535; administrative SSH stays untouched")
	}
	ip := net.ParseIP(host)
	_, tailnet, _ := net.ParseCIDR("100.64.0.0/10")
	_, tailnet6, _ := net.ParseCIDR("fd7a:115c:a1e0::/48")
	if ip == nil || (!ip.IsLoopback() && !tailnet.Contains(ip) && !tailnet6.Contains(ip)) {
		return errors.New("listen must bind a loopback or Tailscale IP, never a public or wildcard interface")
	}
	return nil
}
