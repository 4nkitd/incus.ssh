// Package setup prepares the Incus project and profile used by the gateway.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"incus-for-devs/internal/install"
)

const (
	maxFileBytes   = 64 << 10
	maxOutputBytes = 1 << 20
	defaultImage   = "ifd-ubuntu-24-04"
)

type options struct {
	config    string
	templates string
	project   string
	profile   string
	pool      string
	network   string
	image     string
	binary    string
	apply     bool
}

// Run prepares Incus resources, or explicitly invokes dependency installation.
// It never installs the gateway service or creates storage, networks or images.
func Run(args []string) error {
	opts := options{}
	var installIncus bool
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.StringVar(&opts.config, "config", "", "gateway JSON config to read the image reference from")
	flags.StringVar(&opts.templates, "templates", "deploy", "directory containing project.yaml and profile.yaml")
	flags.StringVar(&opts.project, "project", "ifd-dev", "restricted Incus project")
	flags.StringVar(&opts.profile, "profile", "ifd", "fixed profile")
	flags.StringVar(&opts.pool, "pool", "devpool", "storage pool")
	flags.StringVar(&opts.network, "network", "incusbr0", "managed bridge network (V1: bridge only)")
	flags.StringVar(&opts.image, "image", defaultImage, "cached local image alias or fingerprint")
	flags.StringVar(&opts.binary, "incus", "/usr/bin/incus", "Incus CLI path")
	flags.BoolVar(&opts.apply, "apply", false, "create missing resources; requires root and never edits existing ones")
	flags.BoolVar(&installIncus, "install-incus", false, "plan Incus package installation only; add --apply to install")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: incus-for-devs setup [--templates DIR] [--pool NAME] [--network NAME] [--image REF] [--config FILE] [--apply]")
	}
	if installIncus {
		var incompatible string
		flags.Visit(func(f *flag.Flag) {
			if f.Name != "install-incus" && f.Name != "apply" {
				incompatible = f.Name
			}
		})
		if incompatible != "" {
			return fmt.Errorf("--install-incus cannot be combined with preparation flag --%s; install dependencies separately", incompatible)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return install.Run(ctx, install.Options{Apply: opts.apply}, os.Stdout, os.Stderr)
	}
	return run(context.Background(), opts, os.Stdout, os.Stderr)
}

var (
	geteuid         = os.Geteuid
	checkExecutable = trustedIncusExecutable
)

var (
	slugPattern        = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,46}[a-z0-9])?$`)
	imageAliasPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	sizePattern        = regexp.MustCompile(`^([0-9]+)(B|kB|KB|KiB|MB|MiB|GB|GiB|TB|TiB)$`)
	deviceNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func run(ctx context.Context, opts options, out, _ io.Writer) error {
	if err := validateOptions(&opts); err != nil {
		return err
	}
	root := geteuid() == 0
	if root {
		if err := checkExecutable(opts.binary); err != nil {
			return fmt.Errorf("refusing to run an untrusted Incus executable: %w", err)
		}
	}
	if opts.apply && !root {
		return errors.New("--apply requires root for Incus project and profile creation")
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	incus := incusRunner{bin: opts.binary}
	server, err := incus.checkServer(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "incus:    %s %s\n", serverName(server), serverVersion(server))

	obs, err := observe(ctx, incus, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "project:  %s (%s)\n", opts.project, stateWord(obs.projectExists))
	if obs.projectExists {
		fmt.Fprintf(out, "profile:  %s (%s)\n", opts.profile, stateWord(obs.profileExists))
	} else {
		fmt.Fprintf(out, "profile:  %s (unknown until the project exists)\n", opts.profile)
	}
	fmt.Fprintf(out, "pool:     %s (exists)\n", opts.pool)
	fmt.Fprintf(out, "network:  %s (%s, exists)\n", opts.network, obs.network.Type)
	fmt.Fprintf(out, "image:    %s (%s)\n", opts.image, obs.image.description())

	if !opts.apply {
		fmt.Fprintln(out, "plan only; re-run with --apply as root to create missing project and profile resources (existing resources are never edited)")
		return nil
	}
	return applyPlan(ctx, incus, opts, obs, out)
}

func validateOptions(opts *options) error {
	if !filepath.IsAbs(opts.binary) || strings.ContainsAny(opts.binary, "\x00\r\n") {
		return errors.New("--incus must be an absolute trusted path")
	}
	if opts.templates != "deploy" && !filepath.IsAbs(opts.templates) {
		return errors.New("--templates must be an absolute path or deploy")
	}
	if !validSlug(opts.project) || opts.project == "default" {
		return errors.New("--project must be a lowercase slug of at most 48 characters that starts with a letter, ends with a letter or digit, and is not default")
	}
	if !validSlug(opts.profile) || opts.profile == "default" {
		return errors.New("--profile must be a lowercase slug of at most 48 characters that starts with a letter, ends with a letter or digit, and is not default")
	}
	if !validSlug(opts.pool) {
		return errors.New("--pool must be a lowercase slug of at most 48 characters that starts with a letter and ends with a letter or digit")
	}
	if !validSlug(opts.network) {
		return errors.New("--network must be a lowercase slug of at most 48 characters that starts with a letter and ends with a letter or digit")
	}
	if opts.config != "" {
		image, err := imageFromConfig(opts.config)
		if err != nil {
			return err
		}
		opts.image = image
	}
	if _, err := parseImageRef(opts.image); err != nil {
		return err
	}
	return nil
}

func validSlug(value string) bool {
	return len(value) <= 48 && slugPattern.MatchString(value)
}

func stateWord(exists bool) string {
	if exists {
		return "exists"
	}
	return "missing"
}

func serverName(s serverState) string {
	if s.Environment.Server != "" {
		return s.Environment.Server
	}
	return "incus"
}

func serverVersion(s serverState) string {
	if s.Environment.ServerVersion != "" {
		return s.Environment.ServerVersion
	}
	return "(unknown version)"
}

// imageRef is a locally cached image, referenced either by alias or
// fingerprint. setup never downloads or copies images.
type imageRef struct {
	kind string
	name string
}

func parseImageRef(value string) (imageRef, error) {
	if value == "" || len(value) > 256 {
		return imageRef{}, errors.New("image reference is empty or too long")
	}
	v := value
	if strings.HasPrefix(v, "local:") {
		v = strings.TrimPrefix(v, "local:")
	} else if strings.Contains(v, ":") {
		return imageRef{}, fmt.Errorf("remote image reference %q is not supported; cache the image locally and pass its alias (for example %s) or fingerprint", value, defaultImage)
	}
	if strings.ContainsAny(v, " \t\r\n\x00") || strings.Contains(v, "..") || strings.Contains(v, "//") || strings.HasPrefix(v, "/") || strings.HasSuffix(v, "/") {
		return imageRef{}, errors.New("invalid image reference")
	}
	if fingerprintPattern.MatchString(v) {
		return imageRef{kind: "fingerprint", name: strings.ToLower(v)}, nil
	}
	if !imageAliasPattern.MatchString(v) {
		return imageRef{}, errors.New("image must be a cached local alias without a slash or a 64-character fingerprint")
	}
	return imageRef{kind: "alias", name: v}, nil
}

func imageFromConfig(path string) (string, error) {
	data, err := safeRead(path, "config")
	if err != nil {
		return "", err
	}
	var cfg struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.Image == "" {
		return "", errors.New("config has no valid image field")
	}
	return cfg.Image, nil
}

func templates(dir string) ([]byte, []byte, error) {
	project, err := safeRead(filepath.Join(dir, "project.yaml"), "project template")
	if err != nil {
		return nil, nil, err
	}
	profile, err := safeRead(filepath.Join(dir, "profile.yaml"), "profile template")
	if err != nil {
		return nil, nil, err
	}
	return project, profile, nil
}

// safeRead reads a bounded, regular, non-symlink file that is not writable by
// group or others. Template and config contents influence root-run Incus
// changes, so the checks are deliberately conservative.
func safeRead(path, what string) ([]byte, error) {
	if strings.ContainsAny(path, "\x00\r\n") {
		return nil, fmt.Errorf("%s path is invalid", what)
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s must not be a symlink", what)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s must be a regular file", what)
		}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", what)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s must not be writable by group or others", what)
	}
	if info.Size() > maxFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", what, maxFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", what, maxFileBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", what)
	}
	return data, nil
}

// trustedIncusExecutable refuses to execute a root-run Incus CLI that lives on
// a writable or untrusted path.
func trustedIncusExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("path must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("path must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New("path must be executable")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("path must not be writable by group or others")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("path must be owned by root")
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		dinfo, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("directory %s: %w", dir, err)
		}
		if dinfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("directory %s must not be a symlink", dir)
		}
		if !dinfo.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		if dinfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("directory %s must not be writable by group or others", dir)
		}
		dstat, ok := dinfo.Sys().(*syscall.Stat_t)
		if !ok || dstat.Uid != 0 {
			return fmt.Errorf("directory %s must be owned by root", dir)
		}
		if dir == "/" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return nil
}

type serverState struct {
	Environment struct {
		Server        string `json:"server"`
		ServerVersion string `json:"server_version"`
	} `json:"environment"`
	Auth string `json:"auth"`
}

type projectState struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Config      map[string]string `json:"config"`
}

type profileState struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Config      map[string]string            `json:"config"`
	Devices     map[string]map[string]string `json:"devices"`
}

type poolState struct {
	Name string `json:"name"`
}

type networkState struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Managed bool   `json:"managed"`
}

type imageState struct {
	Fingerprint string `json:"fingerprint"`
	Type        string `json:"type"`
}

type imageAliasState struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

func (i imageState) description() string {
	if i.Type == "" {
		return "cached"
	}
	return "cached " + i.Type + " image " + i.Fingerprint
}

type observedState struct {
	projectExists bool
	project       projectState
	profileExists bool
	profile       profileState
	pool          poolState
	network       networkState
	image         imageState
}

func observe(ctx context.Context, incus incusRunner, opts options) (observedState, error) {
	var obs observedState
	ref, _ := parseImageRef(opts.image)

	projectPath := "/1.0/projects/" + url.PathEscape(opts.project)
	exists, err := incus.get(ctx, projectPath, &obs.project)
	if err != nil {
		return obs, fmt.Errorf("cannot check project %q: %w", opts.project, err)
	}
	obs.projectExists = exists
	if exists && obs.project.Name != "" && obs.project.Name != opts.project {
		return obs, fmt.Errorf("project query returned unexpected name %q", obs.project.Name)
	}

	if exists {
		profilePath := profileAPIPath(opts.project, opts.profile)
		exists, err = incus.get(ctx, profilePath, &obs.profile)
		if err != nil {
			return obs, fmt.Errorf("cannot check profile %q in project %q: %w", opts.profile, opts.project, err)
		}
		obs.profileExists = exists
		if exists && obs.profile.Name != "" && obs.profile.Name != opts.profile {
			return obs, fmt.Errorf("profile query returned unexpected name %q", obs.profile.Name)
		}
	}

	exists, err = incus.get(ctx, "/1.0/storage-pools/"+url.PathEscape(opts.pool), &obs.pool)
	if err != nil {
		return obs, fmt.Errorf("cannot check storage pool %q: %w", opts.pool, err)
	}
	if !exists {
		return obs, fmt.Errorf("storage pool %q is missing; create it before running setup", opts.pool)
	}
	if obs.pool.Name != "" && obs.pool.Name != opts.pool {
		return obs, fmt.Errorf("storage pool query returned unexpected name %q", obs.pool.Name)
	}

	exists, err = incus.get(ctx, "/1.0/networks/"+url.PathEscape(opts.network), &obs.network)
	if err != nil {
		return obs, fmt.Errorf("cannot check network %q: %w", opts.network, err)
	}
	if !exists {
		return obs, fmt.Errorf("network %q is missing; create it before running setup", opts.network)
	}
	if obs.network.Name != "" && obs.network.Name != opts.network {
		return obs, fmt.Errorf("network query returned unexpected name %q", obs.network.Name)
	}
	switch obs.network.Type {
	case "bridge":
	default:
		return obs, fmt.Errorf("network %q has type %q; setup V1 requires a managed bridge network (OVN is not supported)", opts.network, obs.network.Type)
	}
	if !obs.network.Managed {
		return obs, fmt.Errorf("network %q is not managed by Incus; setup requires a managed bridge", opts.network)
	}

	image, err := incus.cachedImage(ctx, ref)
	if err != nil {
		return obs, err
	}
	obs.image = image
	return obs, nil
}

func profileAPIPath(project, profile string) string {
	return "/1.0/profiles/" + url.PathEscape(profile) + "?project=" + url.QueryEscape(project)
}

// incusRunner executes the local Incus CLI. Raw query calls return typed JSON
// and distinguish a missing object from authentication or transport failures.
type incusRunner struct {
	bin string
}

type queryError struct {
	path    string
	detail  string
	missing bool
}

func (e *queryError) Error() string {
	return fmt.Sprintf("query %s: %s", e.path, e.detail)
}

type cmdResult struct {
	stdout    []byte
	stderr    []byte
	code      int
	truncated bool
	err       error
}

func (r incusRunner) exec(ctx context.Context, args ...string) cmdResult {
	full := append([]string{"--force-local"}, args...)
	cmd := exec.CommandContext(ctx, r.bin, full...)
	cmd.Env = commandEnv()
	stdout, stderr := &capBuffer{}, &capBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Stdin = bytes.NewReader(nil)
	err := cmd.Run()
	result := cmdResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), truncated: stdout.truncated || stderr.truncated, err: err}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.code = exitErr.ExitCode()
		if result.code == 0 {
			result.code = 1
		}
		return result
	}
	result.code = -1
	return result
}

// commandEnv removes Incus client overrides so a root-run setup cannot be
// redirected to another daemon through the operator environment.
func commandEnv() []string {
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			name = entry[:i]
		}
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "INCUS_") || strings.HasPrefix(upper, "LXD_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (r incusRunner) query(ctx context.Context, method, path string, body any) ([]byte, error) {
	args := []string{"query"}
	if method != "" && method != "GET" {
		args = append(args, "--wait", "-X", method)
	}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request for %s: %w", path, err)
		}
		args = append(args, "--data", string(raw))
	}
	args = append(args, path)

	result := r.exec(ctx, args...)
	if result.code < 0 {
		detail := commandDetail(result)
		if result.err != nil {
			detail = result.err.Error()
		}
		return nil, fmt.Errorf("cannot run Incus CLI %q: %s", r.bin, detail)
	}
	if result.code != 0 {
		detail := commandDetail(result)
		return nil, &queryError{path: path, detail: detail, missing: isMissing(result.stderr)}
	}
	if result.truncated {
		return nil, fmt.Errorf("query %s returned an oversized response", path)
	}
	return result.stdout, nil
}

func (r incusRunner) get(ctx context.Context, path string, out any) (bool, error) {
	data, err := r.query(ctx, "GET", path, nil)
	if err != nil {
		var qerr *queryError
		if errors.As(err, &qerr) && qerr.missing {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, fmt.Errorf("query %s returned invalid JSON: %w", path, err)
	}
	return true, nil
}

func (r incusRunner) create(ctx context.Context, path string, body any) error {
	_, err := r.query(ctx, "POST", path, body)
	return err
}

func (r incusRunner) checkServer(ctx context.Context) (serverState, error) {
	var state serverState
	exists, err := r.get(ctx, "/1.0", &state)
	if err != nil {
		return state, fmt.Errorf("cannot query the Incus daemon: %w", err)
	}
	if !exists {
		return state, errors.New("the Incus daemon returned no server information")
	}
	if state.Auth != "" && state.Auth != "trusted" {
		return state, fmt.Errorf("the Incus daemon does not trust this client (auth %q); run setup with access to the local Incus socket", state.Auth)
	}
	if state.Environment.Server == "" && state.Environment.ServerVersion == "" {
		return state, errors.New("the Incus daemon returned an unexpected server information response")
	}
	return state, nil
}

func (r incusRunner) cachedImage(ctx context.Context, ref imageRef) (imageState, error) {
	if ref.kind == "fingerprint" {
		var image imageState
		exists, err := r.get(ctx, "/1.0/images/"+url.PathEscape(ref.name), &image)
		if err != nil {
			return image, fmt.Errorf("cannot check image %q: %w", ref.name, err)
		}
		if !exists {
			return image, fmt.Errorf("image %q is not cached locally; cache it on the default project before running setup (setup never downloads images)", ref.name)
		}
		if !strings.EqualFold(image.Fingerprint, ref.name) {
			return image, fmt.Errorf("image query for %q returned fingerprint %q", ref.name, image.Fingerprint)
		}
		if image.Type != "" && image.Type != "container" {
			return image, fmt.Errorf("image %q is a %s image, not a container image", ref.name, image.Type)
		}
		return image, nil
	}

	var alias imageAliasState
	exists, err := r.get(ctx, "/1.0/images/aliases/"+url.PathEscape(ref.name), &alias)
	if err != nil {
		return imageState{}, fmt.Errorf("cannot check image alias %q: %w", ref.name, err)
	}
	if !exists {
		return imageState{}, fmt.Errorf("image alias %q is not cached locally; cache it on the default project before running setup (setup never downloads images)", ref.name)
	}
	if alias.Name != "" && alias.Name != ref.name {
		return imageState{}, fmt.Errorf("image alias query returned unexpected name %q", alias.Name)
	}
	if !fingerprintPattern.MatchString(alias.Target) {
		return imageState{}, fmt.Errorf("image alias %q has an invalid target %q", ref.name, alias.Target)
	}
	var image imageState
	exists, err = r.get(ctx, "/1.0/images/"+url.PathEscape(strings.ToLower(alias.Target)), &image)
	if err != nil {
		return imageState{}, fmt.Errorf("cannot check image %q behind alias %q: %w", alias.Target, ref.name, err)
	}
	if !exists {
		return imageState{}, fmt.Errorf("image alias %q points to uncached image %q; cache the image before running setup", ref.name, alias.Target)
	}
	if !strings.EqualFold(image.Fingerprint, alias.Target) {
		return imageState{}, fmt.Errorf("image alias %q target does not match image metadata", ref.name)
	}
	if image.Type != "" && image.Type != "container" {
		return imageState{}, fmt.Errorf("image %q is a %s image, not a container image", ref.name, image.Type)
	}
	return image, nil
}

func commandDetail(result cmdResult) string {
	text := strings.TrimSpace(string(result.stderr))
	if text == "" {
		text = strings.TrimSpace(string(result.stdout))
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		text = fmt.Sprintf("exit status %d", result.code)
	}
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

func isMissing(stderr []byte) bool {
	text := strings.ToLower(string(stderr))
	for _, denied := range []string{
		"permission denied", "not authorized", "access denied", "forbidden",
		"unable to connect", "connection refused", "connection reset",
		"no such file or directory", "certificate", "authentication",
	} {
		if strings.Contains(text, denied) {
			return false
		}
	}
	return strings.Contains(text, "not found") || strings.Contains(text, "does not exist") || strings.Contains(text, "no such object")
}

type capBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	remaining := maxOutputBytes - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *capBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

// Templates ----------------------------------------------------------------

type stringMap map[string]string

func (m *stringMap) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("must be a YAML mapping")
	}
	out := stringMap{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			return errors.New("mapping keys must be scalars")
		}
		if value.Kind != yaml.ScalarNode || value.Tag == "!!null" {
			return fmt.Errorf("value for %q must be a non-null scalar", key.Value)
		}
		out[key.Value] = value.Value
	}
	*m = out
	return nil
}

type projectTemplate struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Config      stringMap `yaml:"config"`
}

type profileTemplate struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description"`
	Config      stringMap            `yaml:"config"`
	Devices     map[string]stringMap `yaml:"devices"`
}

type projectPlan struct {
	Name        string
	Description string
	Config      map[string]string
}

type profilePlan struct {
	Name        string
	Description string
	Config      map[string]string
	Devices     map[string]map[string]string
}

var projectRequiredConfig = map[string]string{
	"features.images":                 "false",
	"features.profiles":               "true",
	"features.networks":               "false",
	"features.storage.volumes":        "true",
	"features.storage.buckets":        "false",
	"restricted":                      "true",
	"restricted.containers.privilege": "isolated",
	"restricted.containers.nesting":   "block",
	"restricted.containers.lowlevel":  "block",
	"restricted.devices.disk":         "block",
	"restricted.devices.nic":          "managed",
	"restricted.devices.proxy":        "block",
}

var profileRequiredConfig = map[string]string{
	"security.idmap.isolated": "true",
	"security.privileged":     "false",
	"security.nesting":        "false",
	"boot.autostart":          "false",
}

var projectLimitKeys = map[string]bool{
	"containers":       true,
	"instances":        true,
	"virtual-machines": true,
	"cpu":              true,
	"memory":           true,
	"processes":        true,
}

func desired(opts options) (projectPlan, profilePlan, error) {
	projectData, profileData, err := templates(opts.templates)
	if err != nil {
		return projectPlan{}, profilePlan{}, err
	}
	var projectTpl projectTemplate
	if err := parseTemplate(projectData, map[string]bool{"name": true, "description": true, "config": true}, &projectTpl); err != nil {
		return projectPlan{}, profilePlan{}, fmt.Errorf("project template: %w", err)
	}
	var profileTpl profileTemplate
	if err := parseTemplate(profileData, map[string]bool{"name": true, "description": true, "config": true, "devices": true}, &profileTpl); err != nil {
		return projectPlan{}, profilePlan{}, fmt.Errorf("profile template: %w", err)
	}
	project, err := validateProjectTemplate(projectTpl, opts)
	if err != nil {
		return projectPlan{}, profilePlan{}, err
	}
	profile, err := validateProfileTemplate(profileTpl, opts)
	if err != nil {
		return projectPlan{}, profilePlan{}, err
	}
	return project, profile, nil
}

func parseTemplate(data []byte, allowed map[string]bool, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("document is empty")
		}
		return fmt.Errorf("parse YAML: %w", err)
	}
	if doc.Kind == 0 {
		return errors.New("document is empty")
	}
	if err := checkYAML(&doc); err != nil {
		return err
	}
	root := &doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) != 1 {
			return errors.New("document must contain exactly one YAML value")
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return errors.New("must be a YAML mapping")
	}
	for i := 0; i < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if !allowed[key] {
			return fmt.Errorf("unexpected field %q", key)
		}
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
	case err == nil:
		return errors.New("multiple YAML documents are not allowed")
	default:
		return fmt.Errorf("parse YAML: %w", err)
	}
	if err := root.Decode(out); err != nil {
		return fmt.Errorf("parse YAML: %w", err)
	}
	return nil
}

func checkYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := checkYAML(child); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return errors.New("invalid YAML mapping")
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode {
				return errors.New("YAML mapping keys must be scalars")
			}
			if seen[key.Value] {
				return fmt.Errorf("duplicate YAML key %q at line %d", key.Value, key.Line)
			}
			seen[key.Value] = true
			if err := checkYAML(node.Content[i+1]); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return errors.New("YAML aliases are not allowed")
	}
	return nil
}

func validateProjectTemplate(tpl projectTemplate, opts options) (projectPlan, error) {
	if tpl.Name != "" && tpl.Name != opts.project {
		return projectPlan{}, fmt.Errorf("project template names %q but --project is %q", tpl.Name, opts.project)
	}
	cfg := map[string]string(tpl.Config)
	for _, key := range sortedKeys(projectRequiredConfig) {
		want := projectRequiredConfig[key]
		if got := cfg[key]; got != want {
			return projectPlan{}, fmt.Errorf("project template must set %s=%q (found %q)", key, want, got)
		}
	}
	access, ok := cfg["restricted.networks.access"]
	if !ok {
		return projectPlan{}, fmt.Errorf("project template must allow exactly the managed network %q via restricted.networks.access", opts.network)
	}
	if access != opts.network {
		return projectPlan{}, fmt.Errorf("project template allows network %q but --network is %q", access, opts.network)
	}
	for key := range cfg {
		if _, required := projectRequiredConfig[key]; required {
			continue
		}
		switch {
		case key == "restricted.networks.access":
		case key == "features.networks.zones":
			if cfg[key] != "false" {
				return projectPlan{}, fmt.Errorf("project template %s must be false for gateway projects (found %q)", key, cfg[key])
			}
		case strings.HasPrefix(key, "limits."):
			name := strings.TrimPrefix(key, "limits.")
			if !projectLimitKeys[name] {
				return projectPlan{}, fmt.Errorf("project template contains unsupported limit %q", key)
			}
			if err := validateProjectLimit(key, cfg[key]); err != nil {
				return projectPlan{}, err
			}
		default:
			return projectPlan{}, fmt.Errorf("project template contains unsupported security-relevant setting %q", key)
		}
	}
	out := projectPlan{Name: opts.project, Description: tpl.Description, Config: map[string]string{}}
	for key, value := range cfg {
		out.Config[key] = value
	}
	return out, nil
}

func validateProjectLimit(key, value string) error {
	if key == "limits.memory" {
		if err := validateSize(value); err != nil {
			return fmt.Errorf("project template %s is invalid: %w", key, err)
		}
		return nil
	}
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return fmt.Errorf("project template %s must be a non-negative integer (found %q)", key, value)
	}
	if key == "limits.virtual-machines" {
		if n != 0 {
			return fmt.Errorf("project template %s must be 0 for container-only projects (found %q)", key, value)
		}
		return nil
	}
	if n == 0 {
		return fmt.Errorf("project template %s must be a positive integer (found %q)", key, value)
	}
	return nil
}

func validateProfileTemplate(tpl profileTemplate, opts options) (profilePlan, error) {
	if tpl.Name != "" && tpl.Name != opts.profile {
		return profilePlan{}, fmt.Errorf("profile template names %q but --profile is %q", tpl.Name, opts.profile)
	}
	cfg := map[string]string(tpl.Config)
	for _, key := range sortedKeys(profileRequiredConfig) {
		want := profileRequiredConfig[key]
		if got := cfg[key]; got != want {
			return profilePlan{}, fmt.Errorf("profile template must set %s=%q (found %q)", key, want, got)
		}
	}
	for key := range cfg {
		if _, required := profileRequiredConfig[key]; required {
			continue
		}
		switch key {
		case "limits.cpu", "limits.processes":
			if _, err := strconv.ParseUint(cfg[key], 10, 32); err != nil || cfg[key] == "0" {
				return profilePlan{}, fmt.Errorf("profile template %s must be a positive integer (found %q)", key, cfg[key])
			}
		case "limits.memory":
			if err := validateSize(cfg[key]); err != nil {
				return profilePlan{}, fmt.Errorf("profile template %s is invalid: %w", key, err)
			}
		default:
			return profilePlan{}, fmt.Errorf("profile template contains unsupported security-relevant setting %q", key)
		}
	}
	devices, err := validateProfileDevices(tpl.Devices, opts)
	if err != nil {
		return profilePlan{}, err
	}
	out := profilePlan{Name: opts.profile, Description: tpl.Description, Config: map[string]string{}, Devices: devices}
	for key, value := range cfg {
		out.Config[key] = value
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateSize(value string) error {
	parts := sizePattern.FindStringSubmatch(value)
	if parts == nil {
		return fmt.Errorf("size %q must be a positive integer with a byte unit", value)
	}
	n, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || n == 0 {
		return fmt.Errorf("size %q must be positive", value)
	}
	return nil
}

func validateProfileDevices(devices map[string]stringMap, opts options) (map[string]map[string]string, error) {
	if len(devices) == 0 {
		return nil, errors.New("profile template must define a root disk device and a managed network NIC")
	}
	out := map[string]map[string]string{}
	disks, nics := 0, 0
	for name, raw := range devices {
		if len(name) > 63 || !deviceNamePattern.MatchString(name) {
			return nil, fmt.Errorf("profile template device name %q is invalid", name)
		}
		switch raw["type"] {
		case "disk":
			disks++
			if err := validateDiskDevice(name, raw, opts); err != nil {
				return nil, err
			}
			out[name] = map[string]string{"type": "disk", "path": "/", "pool": opts.pool}
		case "nic":
			nics++
			canonical, err := validateNICDevice(name, raw, opts)
			if err != nil {
				return nil, err
			}
			out[name] = canonical
		default:
			return nil, fmt.Errorf("profile template device %q has type %q; only a root disk and a managed network NIC are allowed", name, raw["type"])
		}
	}
	if disks != 1 {
		return nil, fmt.Errorf("profile template must define exactly one root disk device (found %d)", disks)
	}
	if nics != 1 {
		return nil, fmt.Errorf("profile template must define exactly one managed network NIC device (found %d)", nics)
	}
	return out, nil
}

func validateDiskDevice(name string, raw stringMap, opts options) error {
	for key := range raw {
		switch key {
		case "type", "path", "pool", "name":
		default:
			return fmt.Errorf("profile template disk device %q contains unsupported property %q; host paths and raw mount options are not allowed", name, key)
		}
	}
	if raw["path"] != "/" {
		return fmt.Errorf("profile template disk device %q must mount path / (found %q)", name, raw["path"])
	}
	if raw["pool"] != opts.pool {
		return fmt.Errorf("profile template disk device %q uses pool %q but --pool is %q", name, raw["pool"], opts.pool)
	}
	if deviceName, ok := raw["name"]; ok && deviceName != name {
		return fmt.Errorf("profile template disk device %q declares a different name %q", name, deviceName)
	}
	return nil
}

func validateNICDevice(name string, raw stringMap, opts options) (map[string]string, error) {
	for key := range raw {
		switch key {
		case "type", "network", "name", "security.mac_filtering":
		default:
			return nil, fmt.Errorf("profile template NIC device %q contains unsupported property %q; host interfaces and addresses are not allowed", name, key)
		}
	}
	if raw["network"] != opts.network {
		return nil, fmt.Errorf("profile template NIC device %q uses network %q but --network is %q", name, raw["network"], opts.network)
	}
	if deviceName, ok := raw["name"]; ok && deviceName != name {
		return nil, fmt.Errorf("profile template NIC device %q declares a different name %q", name, deviceName)
	}
	canonical := map[string]string{"type": "nic", "network": opts.network}
	if value, ok := raw["security.mac_filtering"]; ok {
		if value != "true" {
			return nil, fmt.Errorf("profile template NIC device %q must enable security.mac_filtering", name)
		}
		canonical["security.mac_filtering"] = "true"
	}
	return canonical, nil
}

// Comparison ---------------------------------------------------------------

func compareProject(existing projectState, want projectPlan) []string {
	diffs := compareText("description", existing.Description, want.Description)
	return append(diffs, compareStringMap("config", existing.Config, want.Config)...)
}

func compareProfile(existing profileState, want profilePlan) []string {
	diffs := compareText("description", existing.Description, want.Description)
	diffs = append(diffs, compareStringMap("config", existing.Config, want.Config)...)
	return append(diffs, compareDevices(existing.Devices, want.Devices)...)
}

func compareText(field, got, want string) []string {
	if got == want {
		return nil
	}
	return []string{fmt.Sprintf("%s: expected %q, found %q", field, want, got)}
}

func compareStringMap(field string, got, want map[string]string) []string {
	keys := map[string]bool{}
	for key := range got {
		keys[key] = true
	}
	for key := range want {
		keys[key] = true
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	var diffs []string
	for _, key := range sorted {
		wantValue, wantOK := want[key]
		gotValue, gotOK := got[key]
		switch {
		case wantOK && !gotOK:
			diffs = append(diffs, fmt.Sprintf("%s %q: missing (expected %q)", field, key, wantValue))
		case !wantOK && gotOK:
			diffs = append(diffs, fmt.Sprintf("%s %q: unexpected (%q)", field, key, gotValue))
		case wantValue != gotValue:
			diffs = append(diffs, fmt.Sprintf("%s %q: expected %q, found %q", field, key, wantValue, gotValue))
		}
	}
	return diffs
}

func compareDevices(got, want map[string]map[string]string) []string {
	names := map[string]bool{}
	for name := range got {
		names[name] = true
	}
	for name := range want {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var diffs []string
	for _, name := range sorted {
		wantDevice, wantOK := want[name]
		gotDevice, gotOK := got[name]
		switch {
		case wantOK && !gotOK:
			diffs = append(diffs, fmt.Sprintf("device %q: missing", name))
		case !wantOK && gotOK:
			diffs = append(diffs, fmt.Sprintf("device %q: unexpected", name))
		default:
			normalized := map[string]string{}
			for key, value := range gotDevice {
				if key == "name" && value == name {
					continue
				}
				normalized[key] = value
			}
			diffs = append(diffs, compareStringMap(fmt.Sprintf("device %q property", name), normalized, wantDevice)...)
		}
	}
	return diffs
}

func joinDiffs(kind, name string, diffs []string) error {
	if len(diffs) == 0 {
		return nil
	}
	return fmt.Errorf("%s %q does not match the template: %s", kind, name, strings.Join(diffs, "; "))
}

// Apply --------------------------------------------------------------------

type projectCreate struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Config      map[string]string `json:"config"`
}

type profileCreate struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	Config      map[string]string            `json:"config"`
	Devices     map[string]map[string]string `json:"devices"`
}

func applyPlan(ctx context.Context, incus incusRunner, opts options, obs observedState, out io.Writer) error {
	project, profile, err := desired(opts)
	if err != nil {
		return err
	}

	if obs.projectExists {
		if err := joinDiffs("project", opts.project, compareProject(obs.project, project)); err != nil {
			return fmt.Errorf("%w; refusing to overwrite existing live configuration", err)
		}
	}
	if obs.profileExists {
		if err := joinDiffs("profile", opts.profile, compareProfile(obs.profile, profile)); err != nil {
			return fmt.Errorf("%w; refusing to overwrite existing live configuration", err)
		}
	}

	createdProject := false
	if !obs.projectExists {
		body := projectCreate{Name: project.Name, Description: project.Description, Config: project.Config}
		if err := incus.create(ctx, "/1.0/projects", body); err != nil {
			return fmt.Errorf("create project %q: %w; the project may or may not have been created, so re-run setup to check and reconcile", opts.project, err)
		}
		fmt.Fprintf(out, "created project %s\n", opts.project)
		createdProject = true
	} else {
		fmt.Fprintf(out, "project %s already matches\n", opts.project)
	}

	if !obs.profileExists {
		body := profileCreate{Name: profile.Name, Description: profile.Description, Config: profile.Config, Devices: profile.Devices}
		path := "/1.0/profiles?project=" + url.QueryEscape(opts.project)
		if err := incus.create(ctx, path, body); err != nil {
			if createdProject {
				return fmt.Errorf("partial state: project %q was created; profile %q may or may not exist: %w; re-run setup to check and reconcile", opts.project, opts.profile, err)
			}
			return fmt.Errorf("partial state: profile %q may or may not have been created: %w; re-run setup to check and reconcile", opts.profile, err)
		}
		fmt.Fprintf(out, "created profile %s\n", opts.profile)
	} else {
		fmt.Fprintf(out, "profile %s already matches\n", opts.profile)
	}

	return verifyApplied(ctx, incus, opts, project, profile, out)
}

func verifyApplied(ctx context.Context, incus incusRunner, opts options, project projectPlan, profile profilePlan, out io.Writer) error {
	var gotProject projectState
	exists, err := incus.get(ctx, "/1.0/projects/"+url.PathEscape(opts.project), &gotProject)
	if err != nil {
		return fmt.Errorf("verify project %q: %w", opts.project, err)
	}
	if !exists {
		return fmt.Errorf("verification failed: project %q is missing after apply", opts.project)
	}
	if err := joinDiffs("project", opts.project, compareProject(gotProject, project)); err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	var gotProfile profileState
	exists, err = incus.get(ctx, profileAPIPath(opts.project, opts.profile), &gotProfile)
	if err != nil {
		return fmt.Errorf("verify profile %q: %w", opts.profile, err)
	}
	if !exists {
		return fmt.Errorf("verification failed: profile %q is missing after apply", opts.profile)
	}
	if err := joinDiffs("profile", opts.profile, compareProfile(gotProfile, profile)); err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	fmt.Fprintln(out, "incus preparation complete; install the gateway service separately")
	return nil
}
