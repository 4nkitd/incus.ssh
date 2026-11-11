package controlapi

import (
	"errors"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Config struct {
	Listen             string `json:"listen"`
	Origin             string `json:"origin"`
	StateDirectory     string `json:"state_directory"`
	ApplicationsDomain string `json:"applications_domain,omitempty"`
}

func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("api.listen must be a loopback IP:port")
	}
	ip := net.ParseIP(host)
	p, err := strconv.Atoi(port)
	if err != nil || p < 1024 || p > 65535 || ip == nil || !ip.IsLoopback() {
		return errors.New("api.listen must be a loopback IP on port 1024-65535; use Caddy for public HTTPS")
	}
	if !strings.HasPrefix(c.Origin, "https://") || ValidateDomain(strings.TrimPrefix(c.Origin, "https://")) != nil {
		return errors.New("api.origin must be https:// followed by a lowercase public DNS hostname, without port or path")
	}
	if !filepath.IsAbs(c.StateDirectory) || filepath.Clean(c.StateDirectory) != c.StateDirectory || c.StateDirectory == "/" {
		return errors.New("api.state_directory must be an absolute private directory")
	}
	if c.ApplicationsDomain != "" {
		if ValidateDomain(c.ApplicationsDomain) != nil || strings.TrimPrefix(c.Origin, "https://") == c.ApplicationsDomain || strings.HasSuffix(strings.TrimPrefix(c.Origin, "https://"), "."+c.ApplicationsDomain) {
			return errors.New("api.applications_domain must be a separate DNS suffix that does not contain the management hostname")
		}
	}
	return nil
}

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidateDomain(domain string) error {
	if len(domain) > 253 || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return errors.New("domain must be a lowercase DNS hostname, not an IP, URL or wildcard")
	}
	for _, label := range strings.Split(domain, ".") {
		if !domainLabel.MatchString(label) {
			return errors.New("invalid domain label")
		}
	}
	return nil
}
