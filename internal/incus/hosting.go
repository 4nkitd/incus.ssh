package incus

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"incus-for-devs/internal/core"
)

const appImageKey = "user.incus-for-devs.app-image"
const appPortKey = "user.incus-for-devs.http-port"
const appBootKey = "user.incus-for-devs.http-boot"
const developmentBoxKey = "user.incus-for-devs.development-box"

var ociImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var registryLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validOCIRegistry(registry string) bool {
	if len(registry) > 253 {
		return false
	}
	host := registry
	if strings.Contains(registry, ":") {
		var port string
		var err error
		host, port, err = net.SplitHostPort(registry)
		n, parseErr := strconv.Atoi(port)
		if err != nil || parseErr != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !registryLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

func normalizeOCIImage(ref string) (string, error) {
	https := strings.HasPrefix(ref, "https://")
	ref = strings.TrimPrefix(ref, "https://")
	registry := "docker.io"
	first, rest, slash := strings.Cut(ref, "/")
	if slash && (strings.ContainsAny(first, ".:") || first == "localhost" || https) {
		if !validOCIRegistry(first) {
			return "", core.UserError{Message: "Use an HTTPS OCI registry hostname, not an IP address, credentials or a download URL."}
		}
		registry, ref = first, rest
	} else if https {
		return "", core.UserError{Message: "A registry URL must include the repository and optional tag or digest."}
	}
	if len(ref) > 256 || !ociImagePattern.MatchString(ref) {
		return "", core.UserError{Message: "Enter an OCI image such as nginx:alpine or ghcr.io/team/app:tag. Direct archives and HTTP URLs are not supported."}
	}
	if registry == "docker.io" && !strings.Contains(ref, "/") {
		ref = "library/" + ref
	}
	return registry + "/" + ref, nil
}

func (b *Backend) registryRemotes() map[string]string {
	registries := make(map[string]string, len(b.config.OCIRegistries)+1)
	for host, remote := range b.config.OCIRegistries {
		registries[host] = remote
	}
	if b.config.OCIRemote != "" {
		registries["docker.io"] = b.config.OCIRemote
	}
	return registries
}

func (b *Backend) HostingCapabilities(ctx context.Context, p core.Principal) (core.HostingCapabilities, error) {
	result := core.HostingCapabilities{Limits: b.config.Limits, Images: append([]string{}, b.config.OCIImages...)}
	registries := b.registryRemotes()
	result.Registries = make([]string, 0, len(registries))
	for host := range registries {
		result.Registries = append(result.Registries, host)
	}
	sort.Strings(result.Registries)
	if err := validatePrincipal(p); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	data, err := b.run(ctx, p, "query", "/1.0")
	if err != nil {
		return result, err
	}
	var server struct {
		Extensions  []string `json:"api_extensions"`
		Environment struct {
			Version string `json:"server_version"`
		} `json:"environment"`
	}
	if json.Unmarshal(data, &server) != nil {
		return result, errors.New("invalid Incus server metadata")
	}
	result.ServerVersion = safeImageMetadata(server.Environment.Version)
	supported := false
	for _, extension := range server.Extensions {
		if extension == "instance_oci_entrypoint" {
			result.DevelopmentBoxes = true
		}
		if extension == "instance_oci" {
			supported = true
		}
	}
	if !supported {
		result.Reason = "This Incus server does not support Docker/OCI images. Ask the operator to upgrade Incus in a maintenance window."
		return result, nil
	}
	if len(registries) == 0 {
		result.Reason = "App hosting is not enabled. Ask the operator to configure an OCI registry remote."
		return result, nil
	}
	data, err = b.run(ctx, p, "remote", "list", "--format=json")
	if err != nil {
		return result, err
	}
	var remotes map[string]struct {
		Addrs      []string
		Addr       string
		Protocol   string
		Public     bool
		CredHelper string
	}
	if json.Unmarshal(data, &remotes) != nil {
		return result, errors.New("invalid Incus remote metadata")
	}
	for host, name := range registries {
		remote, ok := remotes[name]
		addresses := remote.Addrs
		if len(addresses) == 0 && remote.Addr != "" {
			addresses = []string{remote.Addr}
		}
		if !ok || remote.Protocol != "oci" || !remote.Public || remote.CredHelper != "" || len(addresses) != 1 || strings.TrimSuffix(addresses[0], "/") != "https://"+host {
			result.Reason = "The operator must configure each approved OCI registry as a public HTTPS remote, without a credential helper."
			return result, nil
		}
	}
	result.Available = true
	result.Presets = []core.AppPreset{}
	if result.DevelopmentBoxes && registries["docker.io"] != "" {
		result.Presets = core.DevelopmentPresets()
	}
	return result, nil
}

func (b *Backend) CreateApp(ctx context.Context, p core.Principal, in core.AppOptions) (core.Instance, error) {
	if err := validatePrincipal(p); err != nil {
		return core.Instance{}, err
	}
	if !validName(in.Name) {
		return core.Instance{}, core.UserError{Message: "Choose a lowercase app name, using letters, numbers and hyphens."}
	}
	image, err := normalizeOCIImage(in.Image)
	if err != nil {
		return core.Instance{}, err
	}
	registry, reference, _ := strings.Cut(image, "/")
	remote, approved := b.registryRemotes()[registry]
	if !approved {
		return core.Instance{}, core.UserError{Message: "This registry host has not been approved by the operator. Choose one of the registries shown in the deployment form."}
	}
	if len(in.Environment) > 64 {
		return core.Instance{}, core.UserError{Message: "An app can have at most 64 environment variables."}
	}
	total := 0
	for key, value := range in.Environment {
		total += len(key) + len(value)
		if !envNamePattern.MatchString(key) || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > 4096 || total > 32<<10 {
			return core.Instance{}, core.UserError{Message: "Invalid environment variable. Names use letters, digits and underscores; values are limited to 4 KiB each and 32 KiB total."}
		}
	}
	opts, err := b.resources(core.CreateOptions{Name: in.Name, CPU: in.CPU, Memory: in.Memory, Disk: in.Disk})
	if err != nil {
		return core.Instance{}, core.UserError{Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return core.Instance{}, err
	}
	defer func() { <-b.gate }()
	caps, err := b.HostingCapabilities(ctx, p)
	if err != nil {
		return core.Instance{}, err
	}
	if !caps.Available {
		return core.Instance{}, core.UserError{Message: caps.Reason}
	}
	if in.DevelopmentBox && !caps.DevelopmentBoxes {
		return core.Instance{}, core.UserError{Message: "This Incus release cannot override OCI startup commands. Ask the operator to enable a newer supported release."}
	}
	items, err := b.admissionInstances(ctx, p)
	if err != nil {
		return core.Instance{}, err
	}
	if b.runningMemory == 0 && len(items) >= b.config.Limits.MaxInstances {
		return core.Instance{}, core.UserError{Message: "This project has reached its container limit. Ask the operator for more capacity."}
	}
	for _, item := range items {
		if item.Name == in.Name {
			return core.Instance{}, core.UserError{Message: "That app name is already in use."}
		}
	}
	committed, err := b.committedMemory(items, "")
	if err != nil {
		return core.Instance{}, err
	}
	memory, _ := parseSize(opts.Memory)
	if err := b.admitMemory(opts.Memory, memory, committed); err != nil {
		return core.Instance{}, core.UserError{Message: err.Error()}
	}
	config := map[string]string{
		ownerKey: p.Name, appImageKey: image, "limits.cpu": strconv.Itoa(opts.CPU), "limits.memory": opts.Memory, "limits.processes": strconv.Itoa(opts.Processes),
		"boot.autostart": "false", "security.privileged": "false", "security.nesting": "false", "security.guestapi": "false",
	}
	for key, value := range in.Environment {
		config["environment."+key] = value
	}
	if in.DevelopmentBox {
		config[developmentBoxKey] = "true"
		// Incus expects shell-quoted argv here, not a JSON array.
		config["oci.entrypoint"] = `/bin/sh -c "exec sleep infinity"`
	}
	body := map[string]any{"config": config}
	args := []string{"launch", remote + ":" + reference, in.Name, "--profile", b.config.Profile}
	if b.runningMemory != 0 {
		args[0] = "init"
	}
	if opts.Disk != "" {
		args = append(args, "--device", "root,size="+opts.Disk)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return core.Instance{}, err
	}
	// Secret environment values travel through stdin, never process arguments or diagnostics.
	if _, err := b.runInput(ctx, p, data, args...); err != nil {
		return core.Instance{}, core.UserError{Message: "Deployment did not finish. Refresh the app list before retrying; a stopped app may remain. Check the image name, capacity and registry connectivity."}
	}
	if b.runningMemory != 0 {
		if err := b.startCreated(ctx, p, in.Name); err != nil {
			return core.Instance{}, err
		}
	}
	items, err = b.list(ctx, p)
	if err != nil {
		return core.Instance{}, err
	}
	item, err := findOwned(items, p, in.Name)
	if err != nil {
		return core.Instance{}, err
	}
	return b.summary(item), nil
}

func instanceBootID(item instance) string {
	id := instanceConfig(item)["volatile.uuid"]
	if !uuidPattern.MatchString(id) || item.LastUsedAt.IsZero() {
		return ""
	}
	return id + "@" + item.LastUsedAt.UTC().Format(time.RFC3339Nano)
}

func (b *Backend) PublishApp(ctx context.Context, p core.Principal, name string, port int) error {
	if err := validatePrincipal(p); err != nil {
		return err
	}
	if !validName(name) || port < 0 || port > 65535 {
		return core.UserError{Message: "Choose a port between 1 and 65535, or zero to make the app private."}
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return err
	}
	defer func() { <-b.gate }()
	items, err := b.list(ctx, p)
	if err != nil {
		return err
	}
	item, err := findOwned(items, p, name)
	if err != nil {
		return err
	}
	if item.Config[appImageKey] == "" {
		return core.UserError{Message: "Only apps deployed through this panel can use app hostnames."}
	}
	id := ""
	if port != 0 {
		_, id, err = b.serviceInstance(ctx, p, name)
		if err != nil {
			return err
		}
		if _, err = b.serviceAddress(ctx, p, item); err != nil {
			return err
		}
	}
	_, err = b.run(ctx, p, "config", "set", name, appPortKey+"="+strconv.Itoa(port), appBootKey+"="+id)
	return err
}

func (b *Backend) AppTarget(ctx context.Context, p core.Principal, name string) (core.AppTarget, error) {
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	item, id, err := b.serviceInstance(ctx, p, name)
	if err != nil {
		return core.AppTarget{}, err
	}
	port, err := strconv.Atoi(item.Config[appPortKey])
	if err != nil || port < 1 || port > 65535 || item.Config[appImageKey] == "" || id != item.Config[appBootKey] {
		return core.AppTarget{}, errors.New("app is private or its boot changed")
	}
	return core.AppTarget{Port: port, BootID: id}, nil
}

func (b *Backend) AppLogs(ctx context.Context, p core.Principal, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return "", err
	}
	defer func() { <-b.gate }()
	if err := b.checkOwner(ctx, p, name); err != nil {
		return "", err
	}
	cmd := b.command(ctx, p, "console", name, "--show-log")
	out := &tailBuffer{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = &boundedBuffer{limit: 4096}
	if err := cmd.Run(); err != nil {
		return "", errors.New("console log is unavailable")
	}
	return strings.ToValidUTF8(string(out.data), "�"), nil
}

type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
	} else {
		if drop := len(b.data) + len(p) - b.limit; drop > 0 {
			copy(b.data, b.data[drop:])
			b.data = b.data[:len(b.data)-drop]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
