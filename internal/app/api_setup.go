package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"incus-for-devs/internal/controlapi"
)

const apiConfigPath = "/etc/incus-for-devs/config.json"

func setupAPI(args []string) error {
	flags := flag.NewFlagSet("setup-api", flag.ContinueOnError)
	domain := flags.String("domain", "", "public DNS hostname pointing to this Ubuntu 24.04 server")
	apply := flags.Bool("apply", false, "install Caddy, update gateway config and restart the gateway; disconnects active sessions")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || controlapi.ValidateDomain(*domain) != nil {
		return errors.New("usage: incus-for-devs setup-api --domain api.example.com [--apply]")
	}
	fmt.Fprintf(os.Stdout, "Public REST: https://%s/api/v1\nPublic MCP: https://%s/mcp\n", *domain, *domain)
	fmt.Fprintln(os.Stdout, "Plan: install the Ubuntu Caddy package, configure automatic HTTPS on ports 80/443, and bind REST/MCP to 127.0.0.1:8080.")
	fmt.Fprintln(os.Stdout, "Requires DNS A/AAAA pointing here and inbound TCP 80/443. Does not change DNS or firewall rules. Refuses existing Caddy configurations/services and API configuration.")
	fmt.Fprintln(os.Stdout, "Apply backs up config.json, installs Caddy, and RESTARTS incus-for-devs, disconnecting SSH sessions and revoking active guest shares. Containers remain running. API access stays disabled until api enable over SSH.")
	if !*apply {
		fmt.Fprintln(os.Stdout, "Plan only, no changes or host checks performed. Run with sudo and --apply during an approved maintenance window.")
		return nil
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("setup-api --apply requires root on Ubuntu 24.04 with the standard gateway installation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return applyAPISetup(ctx, *domain, os.Stdout)
}

func caddyfile(domain string) string {
	return fmt.Sprintf(`# Managed by incus-for-devs setup-api. No request/access logging.
{
	admin off
}

%s {
	@management path /api/v1/* /mcp /ui /ui/*
	handle @management {
		reverse_proxy 127.0.0.1:8080
	}
	handle {
		respond "Not found" 404
	}
}
`, domain)
}

func applyAPISetup(ctx context.Context, domain string, out io.Writer) error {
	osRelease, err := os.ReadFile("/etc/os-release")
	if err != nil || !strings.Contains("\n"+string(osRelease), "\nID=ubuntu\n") || !strings.Contains(string(osRelease), "VERSION_ID=\"24.04\"") {
		return errors.New("automatic Caddy setup supports Ubuntu 24.04 only")
	}
	info, err := os.Lstat(apiConfigPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		return errors.New("gateway configuration must be an existing non-symlink, operator-owned regular file")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 {
		return errors.New("gateway configuration must be owned by root")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	running, err := os.Stat(executable)
	if err != nil {
		return err
	}
	installed, err := os.Stat("/usr/local/bin/incus-for-devs")
	if err != nil || !os.SameFile(running, installed) {
		return errors.New("run setup-api using the new binary installed at /usr/local/bin/incus-for-devs; do not configure an older gateway with a separate CLI")
	}
	cfg, err := LoadConfig(apiConfigPath)
	if err != nil {
		return err
	}
	if cfg.API != nil {
		return errors.New("API is already configured; refusing to overwrite it")
	}
	for _, path := range []string{"/etc/caddy", "/usr/bin/caddy", "/etc/systemd/system/caddy.service", apiConfigPath + ".before-api"} {
		if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
			return fmt.Errorf("existing or inaccessible %s; refusing to change an existing/partial installation; use the manual API guide", path)
		}
	}
	run := func(command string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=l"}
		data, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s failed: %w; %s", filepath.Base(command), err, strings.TrimSpace(string(data)))
		}
		return strings.TrimSpace(string(data)), nil
	}
	load, err := run("/usr/bin/systemctl", "show", "--property=LoadState", "--value", "caddy.service")
	if err != nil || load != "not-found" {
		return errors.New("an existing Caddy service was found or could not be inspected; refusing automatic setup")
	}
	if _, err := run("/usr/bin/systemctl", "is-active", "--quiet", "incus-for-devs.service"); err != nil {
		return errors.New("the standard gateway service must be active before API setup")
	}
	for _, transport := range []string{"-ltn", "-lun"} {
		listeners, err := run("/usr/bin/ss", "-H", transport, "( sport = :80 or sport = :443 or sport = :8080 )")
		if err != nil {
			return err
		}
		if listeners != "" {
			return errors.New("port 80, 443 or 8080 is in use; refusing to interrupt an existing service")
		}
	}
	cfg.API = &controlapi.Config{Listen: "127.0.0.1:8080", Origin: "https://" + domain, StateDirectory: "/var/lib/incus-for-devs/api"}
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	original, err := os.ReadFile(apiConfigPath)
	if err != nil {
		return err
	}
	if _, err := run("/usr/bin/apt-get", "update"); err != nil {
		return err
	}
	// Package scripts must not start the default public site before our config is validated.
	if _, err := run("/usr/bin/systemctl", "mask", "caddy.service"); err != nil {
		return err
	}
	unmask := true
	defer func() {
		if unmask {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cleanup, "/usr/bin/systemctl", "unmask", "caddy.service")
			cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C"}
			if err := cmd.Run(); err != nil {
				fmt.Fprintln(out, "Caddy cleanup failed; run sudo systemctl unmask caddy.service before recovery.")
			}
		}
	}()
	if _, err := run("/usr/bin/apt-get", "install", "--yes", "--no-install-recommends", "caddy"); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/caddy/Caddyfile", []byte(caddyfile(domain)), 0644); err != nil {
		return err
	}
	if _, err := run("/usr/bin/caddy", "validate", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"); err != nil {
		return err
	}
	backup, err := os.OpenFile(apiConfigPath+".before-api", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := backup.Write(original)
	closeErr := backup.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := replaceConfig(apiConfigPath, append(data, '\n'), info.Mode().Perm(), int(owner.Gid)); err != nil {
		return err
	}
	fmt.Fprintln(out, "Configuration backed up to "+apiConfigPath+".before-api. Restarting the gateway as requested by --apply.")
	if _, err := run("/usr/bin/systemctl", "restart", "incus-for-devs.service"); err != nil {
		return fmt.Errorf("gateway restart failed; backup is %s.before-api; Caddy remains stopped: %w", apiConfigPath, err)
	}
	if _, err := run("/usr/bin/systemctl", "unmask", "caddy.service"); err != nil {
		return err
	}
	unmask = false
	if _, err := run("/usr/bin/systemctl", "enable", "--now", "caddy.service"); err != nil {
		return err
	}
	fmt.Fprintln(out, "Caddy installed and service start requested. Certificate issuance and public reachability are NOT verified. Check HTTPS, then connect over SSH and run api enable to generate your token.")
	return nil
}

func replaceConfig(path string, data []byte, mode os.FileMode, gid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".config-api-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chown(0, gid); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
