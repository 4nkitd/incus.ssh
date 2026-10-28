package incus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"incus-for-devs/internal/core"
)

const ownerKey = "user.incus-for-devs.owner"

var (
	slugPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$|^[a-z]$`)
	imagePattern = regexp.MustCompile(`^(?:[a-z][a-z0-9-]*:)?[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)
	sizePattern  = regexp.MustCompile(`^([0-9]+)(B|kB|KB|KiB|MB|MiB|GB|GiB|TB|TiB)$`)
)

type Config struct {
	Binary           string
	Image            string
	Profile          string
	Limits           core.Limits
	OperationTimeout time.Duration
}

type Backend struct {
	config Config
	memory uint64
	disk   uint64
	env    []string
	gate   chan struct{}
}

var _ core.Backend = (*Backend)(nil)

func New(config Config) (*Backend, error) {
	if !filepath.IsAbs(config.Binary) || strings.ContainsAny(config.Binary, "\x00\r\n") {
		return nil, errors.New("incus binary must be an absolute path")
	}
	if len(config.Image) > 256 || !imagePattern.MatchString(config.Image) || strings.Contains(config.Image, "..") || strings.Contains(config.Image, "//") || strings.HasSuffix(config.Image, "/") {
		return nil, errors.New("invalid administrator image")
	}
	if !validSlug(config.Profile) {
		return nil, errors.New("invalid administrator profile")
	}
	if config.Limits.CPU <= 0 || config.Limits.MaxInstances <= 0 {
		return nil, errors.New("CPU and maximum instance limits must be positive")
	}
	memory, err := parseSize(config.Limits.Memory)
	if err != nil {
		return nil, errors.New("invalid administrator memory limit")
	}
	config.Limits.Memory = canonicalSize(config.Limits.Memory)
	var disk uint64
	if config.Limits.Disk != "" {
		disk, err = parseSize(config.Limits.Disk)
		if err != nil {
			return nil, errors.New("invalid administrator disk limit")
		}
		config.Limits.Disk = canonicalSize(config.Limits.Disk)
	}
	if config.OperationTimeout < 0 {
		return nil, errors.New("operation timeout must not be negative")
	}
	if config.OperationTimeout == 0 {
		config.OperationTimeout = 30 * time.Second
	}
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) && !strings.ContainsAny(home, "\x00\r\n") {
		env = append(env, "HOME="+home)
	}
	return &Backend{config: config, memory: memory, disk: disk, env: env, gate: make(chan struct{}, 1)}, nil
}

func validSlug(value string) bool {
	return len(value) <= 63 && slugPattern.MatchString(value)
}

func validName(name string) bool {
	return validSlug(name)
}

func validatePrincipal(principal core.Principal) error {
	if !validSlug(principal.Name) || !validSlug(principal.Project) {
		return errors.New("invalid principal or project")
	}
	return nil
}

func parseSize(value string) (uint64, error) {
	parts := sizePattern.FindStringSubmatch(value)
	if parts == nil {
		return 0, errors.New("size must be a positive integer with a byte unit")
	}
	units := map[string]uint64{"B": 1, "kB": 1000, "KB": 1000, "KiB": 1 << 10, "MB": 1000 * 1000, "MiB": 1 << 20, "GB": 1000 * 1000 * 1000, "GiB": 1 << 30, "TB": 1000 * 1000 * 1000 * 1000, "TiB": 1 << 40}
	number, err := strconv.ParseUint(parts[1], 10, 64)
	unit := units[parts[2]]
	if err != nil || number == 0 || number > math.MaxInt64/unit {
		return 0, errors.New("size is zero or exceeds supported range")
	}
	return number * unit, nil
}

func canonicalSize(value string) string {
	return strings.Replace(value, "KB", "kB", 1)
}

func (b *Backend) resources(options core.CreateOptions) (core.CreateOptions, error) {
	if options.CPU < 0 || options.CPU > b.config.Limits.CPU {
		return options, errors.New("CPU request exceeds administrator limit or is negative")
	}
	if options.CPU == 0 {
		options.CPU = b.config.Limits.CPU
	}
	if options.Memory == "" {
		options.Memory = b.config.Limits.Memory
	}
	memory, err := parseSize(options.Memory)
	if err != nil || memory > b.memory {
		return options, errors.New("invalid memory request or administrator limit exceeded")
	}
	options.Memory = canonicalSize(options.Memory)
	if b.disk == 0 {
		if options.Disk != "" {
			return options, errors.New("disk quota is unavailable")
		}
	} else {
		if options.Disk == "" {
			options.Disk = b.config.Limits.Disk
		}
		disk, err := parseSize(options.Disk)
		if err != nil || disk > b.disk {
			return options, errors.New("invalid disk request or administrator limit exceeded")
		}
		options.Disk = canonicalSize(options.Disk)
	}
	return options, nil
}

type instance struct {
	Name            string                       `json:"name"`
	Project         string                       `json:"project"`
	Type            string                       `json:"type"`
	Status          string                       `json:"status"`
	Config          map[string]string            `json:"config"`
	ExpandedConfig  map[string]string            `json:"expanded_config"`
	Devices         map[string]map[string]string `json:"devices"`
	ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
}

func owned(item instance, principal core.Principal) bool {
	return validName(item.Name) && item.Type == "container" && (item.Project == "" || item.Project == principal.Project) && item.Config[ownerKey] == principal.Name
}

func (b *Backend) summary(item instance) core.Instance {
	result := core.Instance{Name: item.Name, Status: "Unknown"}
	switch item.Status {
	case "Running", "Stopped", "Frozen", "Starting", "Stopping", "Aborting", "Error", "Ready":
		result.Status = item.Status
	}
	config := item.ExpandedConfig
	if config == nil {
		config = item.Config
	}
	if cpu, err := strconv.Atoi(config["limits.cpu"]); err == nil && cpu > 0 {
		result.CPU = strconv.Itoa(cpu)
	}
	if _, err := parseSize(config["limits.memory"]); err == nil {
		result.Memory = canonicalSize(config["limits.memory"])
	}
	if b.disk != 0 {
		devices := item.ExpandedDevices
		if devices == nil {
			devices = item.Devices
		}
		for _, device := range devices {
			if device["type"] == "disk" && device["path"] == "/" {
				if _, err := parseSize(device["size"]); err == nil {
					result.Disk = canonicalSize(device["size"])
				}
			}
		}
	}
	return result
}

func (b *Backend) List(ctx context.Context, principal core.Principal) ([]core.Instance, error) {
	if err := validatePrincipal(principal); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	items, err := b.list(ctx, principal)
	if err != nil {
		return nil, err
	}
	result := make([]core.Instance, 0, len(items))
	for _, item := range items {
		if owned(item, principal) {
			result = append(result, b.summary(item))
		}
	}
	return result, nil
}

func (b *Backend) list(ctx context.Context, principal core.Principal) ([]instance, error) {
	output, err := b.run(ctx, principal, "list", "--format=json")
	if err != nil {
		return nil, err
	}
	var items []instance
	if err := json.Unmarshal(output, &items); err != nil {
		return nil, errors.New("incus returned invalid instance metadata")
	}
	return items, nil
}

func (b *Backend) checkOwner(ctx context.Context, principal core.Principal, name string) error {
	if err := validatePrincipal(principal); err != nil {
		return err
	}
	if !validName(name) {
		return errors.New("invalid or reserved instance name")
	}
	items, err := b.list(ctx, principal)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Name == name && owned(item, principal) {
			return nil
		}
	}
	return errors.New("instance not found")
}

func (b *Backend) lock(ctx context.Context) error {
	select {
	case b.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) Create(ctx context.Context, principal core.Principal, options core.CreateOptions) (core.Instance, error) {
	if err := validatePrincipal(principal); err != nil {
		return core.Instance{}, err
	}
	if options.Name != "" && !validName(options.Name) {
		return core.Instance{}, errors.New("invalid or reserved instance name")
	}
	options, err := b.resources(options)
	if err != nil {
		return core.Instance{}, err
	}
	if options.Name == "" {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return core.Instance{}, errors.New("cannot generate instance name")
		}
		options.Name = "dev-" + hex.EncodeToString(random[:])
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return core.Instance{}, err
	}
	defer func() { <-b.gate }()
	items, err := b.list(ctx, principal)
	if err != nil {
		return core.Instance{}, err
	}
	// The configured cap covers all instances in this project, across all owners.
	// Separate backend processes still require deployment-enforced project limits.
	if len(items) >= b.config.Limits.MaxInstances {
		return core.Instance{}, errors.New("project instance limit reached")
	}
	for _, item := range items {
		if item.Name == options.Name {
			return core.Instance{}, errors.New("instance name unavailable")
		}
	}
	args := []string{"launch", b.config.Image, options.Name, "--profile", b.config.Profile,
		"--config", ownerKey + "=" + principal.Name,
		"--config", "limits.cpu=" + strconv.Itoa(options.CPU),
		"--config", "limits.memory=" + options.Memory,
		"--config", "limits.processes=256",
		"--config", "boot.autostart=false",
		"--config", "security.privileged=false",
		"--config", "security.nesting=false"}
	if options.Disk != "" {
		args = append(args, "--device", "root,size="+options.Disk)
	}
	if _, err := b.run(ctx, principal, args...); err != nil {
		return core.Instance{}, err
	}
	return core.Instance{Name: options.Name, Status: "Running", CPU: strconv.Itoa(options.CPU), Memory: options.Memory, Disk: options.Disk}, nil
}

func (b *Backend) Remove(ctx context.Context, principal core.Principal, name string) error {
	return b.lifecycle(ctx, principal, name, "delete", "--force")
}

func (b *Backend) Resume(ctx context.Context, principal core.Principal, name string) error {
	return b.lifecycle(ctx, principal, name, "start")
}

func (b *Backend) lifecycle(ctx context.Context, principal core.Principal, name, action string, flags ...string) error {
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return err
	}
	defer func() { <-b.gate }()
	if err := b.checkOwner(ctx, principal, name); err != nil {
		return err
	}
	_, err := b.run(ctx, principal, append([]string{action, name}, flags...)...)
	return err
}

func (b *Backend) command(ctx context.Context, principal core.Principal, args ...string) *exec.Cmd {
	args = append([]string{"--force-local", "--project", principal.Project, "--quiet"}, args...)
	cmd := exec.CommandContext(ctx, b.config.Binary, args...)
	cmd.Env = append([]string(nil), b.env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = time.Second
	return cmd
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

type boundedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	n := len(data)
	remaining := b.limit - len(b.data)
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, data...)
	return n, nil
}

func (b *Backend) run(ctx context.Context, principal core.Principal, args ...string) ([]byte, error) {
	cmd := b.command(ctx, principal, args...)
	stdout := &boundedBuffer{limit: 4 << 20}
	stderr := &boundedBuffer{limit: 8 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("incus %s failed (exit %d)", args[0], exit.ExitCode())
		}
		return nil, fmt.Errorf("incus %s could not complete", args[0])
	}
	if stdout.truncated {
		return nil, errors.New("incus metadata exceeds output limit")
	}
	return stdout.data, nil
}
