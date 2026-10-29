package incus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/names"
)

const ownerKey = "user.incus-for-devs.owner"

const (
	defaultCPU       = 1
	defaultMemory    = 2 << 30
	defaultProcesses = 256
	memoryReserve    = 1 << 30
	maxImageRefBytes = 256
)

var (
	slugPattern        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$|^[a-z]$`)
	imagePattern       = regexp.MustCompile(`^(?:[a-z][a-z0-9-]*:)?[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	imageRefPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	sizePattern        = regexp.MustCompile(`^([0-9]+)(B|kB|KB|KiB|MB|MiB|GB|GiB|TB|TiB)$`)
)

type Config struct {
	Binary           string
	Image            string
	Profile          string
	Limits           core.Limits
	OperationTimeout time.Duration
	// AvailableMemory reports host MemAvailable for new containers; admission
	// keeps memoryReserve free. Nil reads /proc/meminfo.
	AvailableMemory func() (uint64, error)
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
	if config.Limits.Processes < 0 {
		return nil, errors.New("process limit must not be negative")
	}
	if config.Limits.Processes == 0 {
		config.Limits.Processes = defaultProcesses
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
	if config.AvailableMemory == nil {
		config.AvailableMemory = hostAvailableMemory
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

func hostAvailableMemory() (uint64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return parseMemAvailable(data)
}

func parseMemAvailable(data []byte) (uint64, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "MemAvailable:" {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, errors.New("MemAvailable has an unexpected unit")
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || value == 0 || value > math.MaxUint64/1024 {
			return 0, errors.New("MemAvailable is invalid")
		}
		return value * 1024, nil
	}
	return 0, errors.New("MemAvailable is missing")
}

func byteSize(value uint64) string {
	for _, unit := range []struct {
		name string
		size uint64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if value >= unit.size {
			return strconv.FormatUint(value/unit.size, 10) + unit.name
		}
	}
	return strconv.FormatUint(value, 10) + "B"
}

func instanceConfig(item instance) map[string]string {
	if item.ExpandedConfig != nil {
		return item.ExpandedConfig
	}
	return item.Config
}

// projectMemory sums the enforceable memory limits of every instance in the
// project except exclude. It fails closed: an unbounded or unparseable limit
// makes the project's committed memory unknowable, so a conservative admission
// check is impossible.
func projectMemory(items []instance, exclude string) (uint64, error) {
	var committed uint64
	for _, item := range items {
		if exclude != "" && item.Name == exclude {
			continue
		}
		raw := instanceConfig(item)["limits.memory"]
		if raw == "" {
			return 0, errors.New("cannot verify project memory: an existing instance has no enforceable memory limit")
		}
		size, err := parseSize(raw)
		if err != nil {
			return 0, errors.New("cannot verify project memory: an existing instance has an invalid memory limit")
		}
		if committed > math.MaxUint64-size {
			return 0, errors.New("project memory allocation overflows")
		}
		committed += size
	}
	return committed, nil
}

func (b *Backend) admitMemory(request string, memory, committed uint64) error {
	available, err := b.config.AvailableMemory()
	if err != nil {
		return fmt.Errorf("cannot verify host memory availability: %w", err)
	}
	if available < memoryReserve {
		return fmt.Errorf("host has %s available, below the %s reserve", byteSize(available), byteSize(memoryReserve))
	}
	budget := available - memoryReserve
	if committed > budget || memory > budget-committed {
		return fmt.Errorf("host cannot admit %s: %s available with %s reserved and %s already assigned", request, byteSize(available), byteSize(memoryReserve), byteSize(committed))
	}
	return nil
}

// admitStart checks that a stopped instance can be started without exceeding
// host capacity. Its own configured limit is excluded from the committed total
// so starting or restarting it does not count that instance twice.
func (b *Backend) admitStart(target instance, items []instance) error {
	config := instanceConfig(target)
	raw := config["limits.memory"]
	if raw == "" {
		return errors.New("cannot verify instance memory: the instance has no enforceable memory limit")
	}
	memory, err := parseSize(raw)
	if err != nil {
		return errors.New("cannot verify instance memory: the instance has an invalid memory limit")
	}
	if memory > b.memory {
		return fmt.Errorf("instance memory limit %s exceeds maximum %s", canonicalSize(raw), b.config.Limits.Memory)
	}
	cpu, err := strconv.Atoi(config["limits.cpu"])
	if err != nil || cpu <= 0 || cpu > b.cpuCeiling() {
		return fmt.Errorf("instance CPU limit must be between 1 and %d", b.cpuCeiling())
	}
	processes, err := strconv.Atoi(config["limits.processes"])
	if err != nil || processes <= 0 || processes > b.config.Limits.Processes {
		return fmt.Errorf("instance process limit must be between 1 and %d", b.config.Limits.Processes)
	}
	// A restart keeps the same limits and releases this guest's resident memory.
	// Requiring its full cap again from MemAvailable would reject busy guests.
	if instanceActive(target.Status) {
		return nil
	}
	committed, err := projectMemory(items, target.Name)
	if err != nil {
		return err
	}
	return b.admitMemory(canonicalSize(raw), memory, committed)
}

func (b *Backend) cpuCeiling() int {
	maxCPU := b.config.Limits.CPU
	if cores := runtime.NumCPU(); cores > 0 && cores < maxCPU {
		maxCPU = cores
	}
	return maxCPU
}

func (b *Backend) resources(options core.CreateOptions) (core.CreateOptions, error) {
	maxCPU := b.cpuCeiling()
	if options.CPU < 0 {
		return options, errors.New("CPU request must not be negative")
	}
	if options.CPU > maxCPU {
		return options, fmt.Errorf("CPU request %d exceeds maximum %d", options.CPU, maxCPU)
	}
	if options.CPU == 0 {
		options.CPU = min(defaultCPU, maxCPU)
	}
	if options.Processes < 0 {
		return options, errors.New("process request must not be negative")
	}
	if options.Processes > b.config.Limits.Processes {
		return options, fmt.Errorf("process request %d exceeds maximum %d", options.Processes, b.config.Limits.Processes)
	}
	if options.Processes == 0 {
		options.Processes = min(defaultProcesses, b.config.Limits.Processes)
	}
	if options.Memory == "" {
		if b.memory <= defaultMemory {
			options.Memory = b.config.Limits.Memory
		} else {
			options.Memory = "2GiB"
		}
	}
	memory, err := parseSize(options.Memory)
	if err != nil {
		return options, errors.New("invalid memory request")
	}
	if memory > b.memory {
		return options, fmt.Errorf("memory request %s exceeds maximum %s", canonicalSize(options.Memory), b.config.Limits.Memory)
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

type imageAlias struct {
	Name string `json:"name"`
}

type imageProperties struct {
	Description string `json:"description"`
	OS          string `json:"os"`
	Release     string `json:"release"`
}

const maxImageMetadataBytes = 1024

// safeImageMetadata drops cached image metadata that could break a terminal or
// an operator's assumptions. Control characters include C0, DEL and C1 (for
// example U+0085), so text containing them is omitted rather than rendered.
func safeImageMetadata(value string) string {
	if len(value) > maxImageMetadataBytes || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return value
}

// image is the bounded, metadata-only view of a cached Incus image. Image
// configuration and environment are deliberately not decoded or exposed.
type image struct {
	Fingerprint  string          `json:"fingerprint"`
	Type         string          `json:"type"`
	Architecture string          `json:"architecture"`
	Aliases      []imageAlias    `json:"aliases"`
	Properties   imageProperties `json:"properties"`
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// validImageToken reports whether value is a bounded native image alias.
// Colons, URL schemes and any path traversal segment are rejected so a
// reference can only ever name a single locally cached image.
func validImageToken(value string) bool {
	if value == "" || len(value) > maxImageRefBytes || strings.HasPrefix(value, "-") || hasControl(value) {
		return false
	}
	if strings.Contains(value, ":") || !imageRefPattern.MatchString(value) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// normalizeImageRef validates a per-request image reference. An optional
// local: prefix is accepted; every other remote reference is rejected because
// requests may only select from the operator-managed local cache. Fingerprints
// are normalized to lowercase.
func normalizeImageRef(value string) (string, bool, error) {
	if value == "" || len(value) > maxImageRefBytes {
		return "", false, errors.New("image reference is empty or too long")
	}
	ref := strings.TrimPrefix(value, "local:")
	if strings.Contains(ref, ":") {
		return "", false, errors.New("remote image references are not allowed")
	}
	if !validImageToken(ref) {
		return "", false, errors.New("invalid image reference")
	}
	if fingerprintPattern.MatchString(ref) {
		return strings.ToLower(ref), true, nil
	}
	return ref, false, nil
}

func (b *Backend) imageList(ctx context.Context, principal core.Principal) ([]image, error) {
	output, err := b.run(ctx, principal, "image", "list", "local:", "--format=json")
	if err != nil {
		return nil, err
	}
	var images []image
	if err := json.Unmarshal(output, &images); err != nil {
		return nil, errors.New("incus returned invalid image metadata")
	}
	return images, nil
}

// Images reports the cached container images visible in the principal's
// project. It is a read-only projection: no download, copy, alias change or
// other operator cache mutation is performed, and VM images are excluded.
func (b *Backend) Images(ctx context.Context, principal core.Principal) ([]core.ImageInfo, error) {
	if err := validatePrincipal(principal); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	images, err := b.imageList(ctx, principal)
	if err != nil {
		return nil, err
	}
	result := make([]core.ImageInfo, 0, len(images))
	for _, item := range images {
		if item.Type != "container" || !fingerprintPattern.MatchString(item.Fingerprint) {
			continue
		}
		aliases := make([]string, 0, len(item.Aliases))
		seen := make(map[string]bool, len(item.Aliases))
		for _, alias := range item.Aliases {
			if !validImageToken(alias.Name) || fingerprintPattern.MatchString(alias.Name) || seen[alias.Name] {
				continue
			}
			seen[alias.Name] = true
			aliases = append(aliases, alias.Name)
		}
		sort.Strings(aliases)
		result = append(result, core.ImageInfo{
			Fingerprint:  strings.ToLower(item.Fingerprint),
			Aliases:      aliases,
			Description:  safeImageMetadata(item.Properties.Description),
			OS:           safeImageMetadata(item.Properties.OS),
			Release:      safeImageMetadata(item.Properties.Release),
			Architecture: safeImageMetadata(item.Architecture),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Fingerprint < result[j].Fingerprint })
	return result, nil
}

// resolveImage pins a requested alias or fingerprint to the exact cached
// container image fingerprint used for launch. The user-supplied string never
// reaches the CLI, and ambiguous or virtual-machine images are rejected.
func (b *Backend) resolveImage(ctx context.Context, principal core.Principal, reference string) (string, error) {
	ref, byFingerprint, err := normalizeImageRef(reference)
	if err != nil {
		return "", err
	}
	images, err := b.imageList(ctx, principal)
	if err != nil {
		return "", err
	}
	matches := make([]image, 0, 1)
	seen := make(map[string]bool, 1)
	for _, candidate := range images {
		if !fingerprintPattern.MatchString(candidate.Fingerprint) {
			continue
		}
		matched := false
		if byFingerprint {
			matched = strings.EqualFold(candidate.Fingerprint, ref)
		} else {
			for _, alias := range candidate.Aliases {
				if alias.Name == ref {
					matched = true
					break
				}
			}
		}
		fingerprint := strings.ToLower(candidate.Fingerprint)
		if !matched || seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		matches = append(matches, candidate)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("image %q is not cached", ref)
	case 1:
	default:
		return "", fmt.Errorf("image %q matches multiple cached images", ref)
	}
	if matches[0].Type != "container" {
		return "", fmt.Errorf("image %q is not a container image", ref)
	}
	return strings.ToLower(matches[0].Fingerprint), nil
}

type instance struct {
	Name            string                       `json:"name"`
	Project         string                       `json:"project"`
	Type            string                       `json:"type"`
	Status          string                       `json:"status"`
	LastUsedAt      time.Time                    `json:"last_used_at"`
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
	config := instanceConfig(item)
	if cpu, err := strconv.Atoi(config["limits.cpu"]); err == nil && cpu > 0 {
		result.CPU = strconv.Itoa(cpu)
	}
	if _, err := parseSize(config["limits.memory"]); err == nil {
		result.Memory = canonicalSize(config["limits.memory"])
	}
	if processes, err := strconv.Atoi(config["limits.processes"]); err == nil && processes > 0 {
		result.Processes = strconv.Itoa(processes)
	}
	// Only the immutable fingerprint recorded by Incus is reported. Aliases are
	// mutable and are never inferred after creation.
	if fingerprint := config["volatile.base_image"]; fingerprintPattern.MatchString(fingerprint) {
		result.Image = strings.ToLower(fingerprint)
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

func findOwned(items []instance, principal core.Principal, name string) (instance, error) {
	if !validName(name) {
		return instance{}, errors.New("invalid or reserved instance name")
	}
	for _, item := range items {
		if item.Name == name && owned(item, principal) {
			return item, nil
		}
	}
	return instance{}, errors.New("instance not found")
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
	_, err = findOwned(items, principal, name)
	return err
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
		return core.Instance{}, errors.New("invalid instance name")
	}
	// Syntactic rejection happens before the create gate so malformed image
	// input can never reach the CLI, even as an argument.
	if options.Image != "" {
		if _, _, err := normalizeImageRef(options.Image); err != nil {
			return core.Instance{}, err
		}
	}
	options, err := b.resources(options)
	if err != nil {
		return core.Instance{}, err
	}
	generated := options.Name == ""
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
	// The local gate serializes only this backend: external incus commands and
	// other backend processes are not serialized, so deploy/project.yaml must
	// enforce the same project limits.
	if len(items) >= b.config.Limits.MaxInstances {
		return core.Instance{}, errors.New("project instance limit reached")
	}
	taken := make(map[string]bool, len(items))
	for _, item := range items {
		taken[item.Name] = true
	}
	if generated {
		options.Name, err = names.Pick(taken)
		if err != nil {
			return core.Instance{}, err
		}
	} else if taken[options.Name] {
		return core.Instance{}, errors.New("instance name unavailable")
	}
	requestedMemory, err := parseSize(options.Memory)
	if err != nil {
		return core.Instance{}, errors.New("invalid memory request")
	}
	committed, err := projectMemory(items, "")
	if err != nil {
		return core.Instance{}, err
	}
	if err := b.admitMemory(options.Memory, requestedMemory, committed); err != nil {
		return core.Instance{}, err
	}
	// limits.memory is a RAM cap only. Do not add limits.memory.swap: Incus 6
	// projects with restricted.containers.lowlevel=block reject that key.
	imageRef := b.config.Image
	if options.Image != "" {
		// The catalog is read inside the create gate and this operation's
		// timeout. Only the resolved fingerprint reaches launch, never the
		// user-supplied alias.
		imageRef, err = b.resolveImage(ctx, principal, options.Image)
		if err != nil {
			return core.Instance{}, err
		}
	}
	args := []string{"launch", imageRef, options.Name, "--profile", b.config.Profile,
		"--config", ownerKey + "=" + principal.Name,
		"--config", "limits.cpu=" + strconv.Itoa(options.CPU),
		"--config", "limits.memory=" + options.Memory,
		"--config", "limits.processes=" + strconv.Itoa(options.Processes),
		"--config", "boot.autostart=false",
		"--config", "security.privileged=false",
		"--config", "security.nesting=false"}
	if options.Disk != "" {
		args = append(args, "--device", "root,size="+options.Disk)
	}
	if _, err := b.run(ctx, principal, args...); err != nil {
		return core.Instance{}, err
	}
	result := core.Instance{Name: options.Name, Status: "Running", CPU: strconv.Itoa(options.CPU), Memory: options.Memory, Processes: strconv.Itoa(options.Processes), Disk: options.Disk}
	if options.Image != "" {
		result.Image = imageRef
	}
	return result, nil
}

func (b *Backend) Remove(ctx context.Context, principal core.Principal, name string) error {
	return b.lifecycle(ctx, principal, name, "delete", "--force")
}

func (b *Backend) Resume(ctx context.Context, principal core.Principal, name string) error {
	return b.lifecycle(ctx, principal, name, "start")
}

func (b *Backend) Start(ctx context.Context, principal core.Principal, name string) error {
	return b.lifecycle(ctx, principal, name, "start")
}

// gracefulFlags requests a clean shutdown. Incus 6 only force-stops with
// --force (which sets its internal timeout to zero); --timeout merely bounds
// the clean-shutdown wait and fails the operation if the guest has not stopped.
// We pass --force=false explicitly and derive the wait from OperationTimeout,
// clamped to at least one second so a sub-second timeout cannot hit Incus's
// zero-timeout force path. If the deadline expires the operation is cancelled
// or fails; it is never reported as a confirmed stop and the guest is not
// force-killed.
func gracefulFlags(timeout time.Duration) []string {
	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return []string{"--force=false", "--timeout=" + strconv.Itoa(seconds)}
}

func (b *Backend) Stop(ctx context.Context, principal core.Principal, name string) error {
	return b.lifecycle(ctx, principal, name, "stop", gracefulFlags(b.config.OperationTimeout)...)
}

func (b *Backend) Restart(ctx context.Context, principal core.Principal, name string) error {
	return b.lifecycle(ctx, principal, name, "restart", gracefulFlags(b.config.OperationTimeout)...)
}

// instanceActive reports whether an instance already has its memory allocated.
// Start of an already-active instance changes no committed resources, so Incus
// decides whether it is a no-op or an error and no admission is needed.
func instanceActive(status string) bool {
	switch status {
	case "Started", "Running", "Starting", "Ready", "Thawed", "Frozen", "Freezing", "Stopping", "Aborting", "Pending", "Cancelling":
		return true
	}
	return false
}

func (b *Backend) lifecycle(ctx context.Context, principal core.Principal, name, action string, flags ...string) error {
	if err := validatePrincipal(principal); err != nil {
		return err
	}
	if !validName(name) {
		return errors.New("invalid or reserved instance name")
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return err
	}
	defer func() { <-b.gate }()
	items, err := b.list(ctx, principal)
	if err != nil {
		return err
	}
	target, err := findOwned(items, principal, name)
	if err != nil {
		return err
	}
	switch action {
	case "start":
		if !instanceActive(target.Status) {
			if err := b.admitStart(target, items); err != nil {
				return err
			}
		}
	case "restart":
		// Revalidate operator ceilings; only stopped guests need fresh capacity.
		if err := b.admitStart(target, items); err != nil {
			return err
		}
	}
	_, err = b.run(ctx, principal, append([]string{action, name}, flags...)...)
	return err
}

func (b *Backend) command(ctx context.Context, principal core.Principal, args ...string) *exec.Cmd {
	prefix := []string{"--force-local", "--project", principal.Project, "--quiet"}
	if len(args) > 0 && args[0] == "query" {
		// Incus 6 rejects --project for raw queries; their API URI carries project scope.
		prefix = []string{"--force-local", "--quiet"}
	}
	args = append(prefix, args...)
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
