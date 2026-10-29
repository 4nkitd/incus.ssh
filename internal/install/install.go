// Package install installs the distribution Incus packages required by
// incus.ssh on a supported Linux host.
//
// Plan mode (Options.Apply false) reads /etc/os-release, checks trusted path
// metadata for /usr/bin/incus and the daemon binary, writes nothing and starts
// no command. Apply mode requires root and uses only the host's already
// configured repositories: it runs the trusted /usr/bin/apt-get update, a
// full bounded install simulation, then exactly
// /usr/bin/apt-get install --no-remove --no-install-recommends --yes incus
// incus-client ca-certificates dnsmasq-base. ca-certificates supplies the
// operating-system CA roots needed for native Incus HTTPS image copy/list, and
// dnsmasq-base provides the executable required when creating a managed Incus
// bridge; it is explicit because --no-install-recommends intentionally avoids
// the wider recommended stack. Dependency packages may be installed or
// upgraded from the configured repositories; any planned removal or purge
// aborts before install.
//
// The installer's own direct actions are limited to reading host metadata,
// verifying trusted binary paths and running those package commands. Package
// maintainer scripts run by apt/dpkg may start or restart Incus, dnsmasq or
// related services and may add subuid/subgid or sysctl entries; those
// distribution-owned effects are outside this package's control.
//
// This package itself does not add repositories, pipe a download into a shell,
// use brew, snap, Docker or the lxc installer as a fallback, run a full
// upgrade, purge packages, initialize Incus, create a storage pool or network,
// cache an image, enable the public listener, or run SSH, firewall, Docker or
// Tailscale commands. Initializing Incus and caching an image remain separate
// operator steps before `setup --apply`.
package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

	"incus-for-devs/internal/host"
)

const (
	osReleasePath = "/etc/os-release"
	incusBinary   = "/usr/bin/incus"
	aptGet        = "/usr/bin/apt-get"

	updateTimeout   = 5 * time.Minute
	simulateTimeout = 2 * time.Minute
	installTimeout  = 20 * time.Minute
	queryTimeout    = 30 * time.Second

	maxOSReleaseBytes = 64 << 10
)

// Ubuntu 24.04 and Debian packages place incusd under /usr/libexec/incus.
// The two other locations are accepted for compatible packaging layouts.
var incusDaemonPaths = []string{
	"/usr/libexec/incus/incusd",
	"/usr/sbin/incusd",
	"/usr/bin/incusd",
}

var requestedPackagePattern = regexp.MustCompile(`(?i)(^|[^a-z0-9+.-])(incus-client|dnsmasq-base|ca-certificates|incus)([^a-z0-9+.-]|$)`)

// Options selects plan or apply mode. Apply performs changes and requires
// root; the parent command maps its explicit --apply flag here.
type Options struct {
	Apply bool
}

var (
	geteuid         = os.Geteuid
	readFile        = readHostFile
	statPath        = os.Lstat
	checkExecutable = host.TrustedExecutable
)

type osInfo struct {
	id        string
	versionID string
	name      string
}

type distribution struct {
	label  string
	source string
	caveat string
}

type existingState struct {
	client     bool
	daemon     bool
	daemonPath string
}

func (s existingState) complete() bool {
	return s.client && s.daemon
}

// Run reports or installs the distribution Incus packages. It does not
// initialize Incus and does not touch the gateway service or configuration.
func Run(ctx context.Context, opts Options, out, errOut io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	if opts.Apply && geteuid() != 0 {
		return errors.New("--apply requires root to install distribution packages; nothing was run, so re-run with root privileges")
	}

	existing, err := inspectExisting()
	if err != nil {
		return err
	}
	info, err := identifyOS()
	if err != nil {
		return err
	}

	if opts.Apply {
		if existing.complete() {
			return verifyExisting(ctx, info, existing, out)
		}
		distro, err := supportedDistro(info)
		if err != nil {
			return err
		}
		return applyPackages(ctx, distro, out, errOut)
	}

	if existing.complete() {
		printExistingPlan(out, info, existing)
		return nil
	}
	distro, err := supportedDistro(info)
	if err != nil {
		return err
	}
	return printPlan(out, distro, existing)
}

func readHostFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxOSReleaseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxOSReleaseBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxOSReleaseBytes)
	}
	return data, nil
}

func identifyOS() (osInfo, error) {
	data, err := readFile(osReleasePath)
	if err != nil {
		return osInfo{}, fmt.Errorf("cannot identify the operating system from %s: %w; this installer supports Ubuntu 24.04 and Debian 12/13 Linux hosts", osReleasePath, err)
	}
	return parseOSRelease(data)
}

var relevantOSKeys = map[string]bool{
	"ID":          true,
	"VERSION_ID":  true,
	"PRETTY_NAME": true,
}

// parseOSRelease reads shell-style KEY=value assignments without sourcing the
// file or starting a shell. Unknown keys are ignored, including malformed
// values. Duplicate ID, VERSION_ID or PRETTY_NAME keys are rejected instead of
// silently taking the first value.
func parseOSRelease(data []byte) (osInfo, error) {
	values := map[string]string{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t") || !relevantOSKeys[key] {
			continue
		}
		if seen[key] {
			return osInfo{}, fmt.Errorf("duplicate %s in %s; refusing an ambiguous operating system identity", key, osReleasePath)
		}
		value, err := parseOSReleaseValue(rawValue)
		if err != nil {
			return osInfo{}, fmt.Errorf("invalid %s in %s: %w", key, osReleasePath, err)
		}
		seen[key] = true
		values[key] = value
	}
	if values["ID"] == "" {
		return osInfo{}, fmt.Errorf("%s has no ID field; this installer supports Ubuntu 24.04 and Debian 12/13 Linux hosts", osReleasePath)
	}
	return osInfo{id: values["ID"], versionID: values["VERSION_ID"], name: values["PRETTY_NAME"]}, nil
}

func parseOSReleaseValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	switch value[0] {
	case '"':
		var text strings.Builder
		escaped := false
		for i := 1; i < len(value); i++ {
			char := value[i]
			if escaped {
				switch char {
				case '"', '\\', '$', '`':
					text.WriteByte(char)
				default:
					text.WriteByte('\\')
					text.WriteByte(char)
				}
				escaped = false
				continue
			}
			switch char {
			case '\\':
				escaped = true
			case '"':
				if strings.TrimSpace(value[i+1:]) != "" {
					return "", errors.New("unexpected text after closing quote")
				}
				return text.String(), nil
			default:
				text.WriteByte(char)
			}
		}
		return "", errors.New("unterminated double quote")
	case '\'':
		end := strings.IndexByte(value[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated single quote")
		}
		if strings.TrimSpace(value[end+2:]) != "" {
			return "", errors.New("unexpected text after closing quote")
		}
		return value[1 : end+1], nil
	default:
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return "", nil
		}
		return fields[0], nil
	}
}

func (o osInfo) label() string {
	if name := strings.TrimSpace(o.name); name != "" {
		return name
	}
	if o.id != "" && o.versionID != "" {
		return o.id + " " + o.versionID
	}
	if o.id != "" {
		return o.id
	}
	return "unknown Linux"
}

func supportedDistro(info osInfo) (distribution, error) {
	switch {
	case info.id == "ubuntu" && info.versionID == "24.04":
		return distribution{
			label:  info.label(),
			source: "official Ubuntu 24.04 archive, already configured (repositories are never added)",
		}, nil
	case info.id == "debian" && (info.versionID == "12" || info.versionID == "13"):
		distro := distribution{
			label:  info.label(),
			source: fmt.Sprintf("configured Debian %s repositories; an incus candidate must already be available (repositories are never added)", info.versionID),
		}
		if info.versionID == "12" {
			distro.caveat = "Debian 12 stock repositories may not provide incus; apply verifies the candidate and stops without installing if it is unavailable"
		}
		return distro, nil
	}
	return distribution{}, fmt.Errorf("unsupported operating system %q (ID=%q VERSION_ID=%q): this installer supports Ubuntu 24.04 and Debian 12/13 only when the configured distribution repositories actually provide incus, and Debian 12 stock repositories may not. No repositories were added, no download script was run and no alternative installer was used. Install Incus from your distribution's documented package source, then re-run `incus-for-devs setup --apply`", info.label(), info.id, info.versionID)
}

func inspectExisting() (existingState, error) {
	var state existingState
	if _, err := statPath(incusBinary); err == nil {
		if err := checkExecutable(incusBinary); err != nil {
			return existingState{}, fmt.Errorf("refusing to use the existing Incus client %s: %w; this installer never replaces or invokes an untrusted binary, and it does not fall back to another installer", incusBinary, err)
		}
		state.client = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return existingState{}, fmt.Errorf("inspect %s: %w", incusBinary, err)
	}

	for _, path := range incusDaemonPaths {
		if _, err := statPath(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return existingState{}, fmt.Errorf("inspect %s: %w", path, err)
		}
		if err := checkExecutable(path); err != nil {
			return existingState{}, fmt.Errorf("refusing to use the existing Incus daemon binary %s: %w; this installer never replaces or invokes an untrusted binary, and it does not fall back to another installer", path, err)
		}
		state.daemon = true
		state.daemonPath = path
		break
	}
	return state, nil
}

func existingNote(state existingState) string {
	switch {
	case state.complete():
		return "trusted client and daemon binaries found (metadata only)"
	case state.client:
		return "client binary only; the daemon binary is missing, so apply installs the distribution packages"
	case state.daemon:
		return fmt.Sprintf("daemon binary only (%s); the client is missing, so apply installs the distribution packages", state.daemonPath)
	default:
		return "no /usr/bin/incus client or daemon binary found at " + strings.Join(incusDaemonPaths, ", ")
	}
}

func printExistingPlan(out io.Writer, info osInfo, state existingState) {
	fmt.Fprintln(out, "incus install plan")
	fmt.Fprintf(out, "os:       %s\n", info.label())
	fmt.Fprintf(out, "existing: %s\n", existingNote(state))
	fmt.Fprintf(out, "daemon:   %s\n", state.daemonPath)
	fmt.Fprintln(out, "plan only; no files were written and no command was run.")
	fmt.Fprintln(out, "apply runs as root: one read-only /usr/bin/incus --force-local query /1.0; if the server metadata is readable, it skips apt and all package updates.")
	fmt.Fprintln(out, "package installation and daemon readiness are reported separately; a missing or unreachable daemon is never reported as a successful no-op.")
	printNextSteps(out)
}

func printPlan(out io.Writer, distro distribution, state existingState) error {
	fmt.Fprintln(out, "incus install plan")
	fmt.Fprintf(out, "os:       %s\n", distro.label)
	fmt.Fprintf(out, "source:   %s\n", distro.source)
	fmt.Fprintf(out, "packages: %s (--no-remove, --no-install-recommends, non-interactive)\n", strings.Join(requestedPackages, " "))
	fmt.Fprintf(out, "existing: %s\n", existingNote(state))
	if distro.caveat != "" {
		fmt.Fprintf(out, "caveat:   %s\n", distro.caveat)
	}
	fmt.Fprintln(out, "apply runs as root, in order:")
	fmt.Fprintf(out, "  %s %s\n", aptGet, strings.Join(updateArgs(), " "))
	fmt.Fprintf(out, "  %s %s\n", aptGet, strings.Join(simulateArgs(), " "))
	fmt.Fprintf(out, "  %s %s\n", aptGet, strings.Join(installArgs(), " "))
	fmt.Fprintln(out, "the full simulation output is bounded and checked for removals or purges before install; dependency packages may be installed or upgraded from configured repositories.")
	fmt.Fprintln(out, "plan only; no files were written and no command was run.")
	fmt.Fprintln(out, "re-run with --apply as root to install the distribution packages.")
	printNextSteps(out)
	return nil
}

func verifyExisting(ctx context.Context, info osInfo, state existingState, out io.Writer) error {
	result := runOne(ctx, queryTimeout, maxQueryOutput, io.Discard, io.Discard, incusBinary, queryArgs(), incusEnv())
	if result.err != nil {
		return daemonUnavailable(state, operationFailure(result))
	}
	if result.truncated {
		return daemonUnavailable(state, fmt.Sprintf("server metadata response exceeded %d bytes", maxQueryOutput))
	}
	metadata, err := parseServerMetadata([]byte(result.stdout))
	if err != nil {
		return daemonUnavailable(state, err.Error())
	}

	server := metadata.Environment.Server
	if server == "" {
		server = "incus"
	}
	version := metadata.Environment.ServerVersion
	if version == "" {
		version = "unknown version"
	}
	fmt.Fprintf(out, "incus:    trusted client %s and daemon %s are installed\n", incusBinary, state.daemonPath)
	fmt.Fprintf(out, "os:       %s\n", info.label())
	fmt.Fprintf(out, "daemon:   reachable; %s %s (read-only server metadata query)\n", server, version)
	fmt.Fprintln(out, "packages: installed; no apt or global package update was run")
	printNextSteps(out)
	return nil
}

func daemonUnavailable(state existingState, detail string) error {
	return fmt.Errorf("Incus packages appear installed at %s (client) and %s (daemon), but the read-only server metadata query failed: %s. Package installation and daemon readiness are separate; the daemon may be stopped, unreachable or untrusted. No apt command was run and no package was changed, so reinstalling is not the fix. Start or repair the Incus daemon separately, then re-run", incusBinary, state.daemonPath, detail)
}

type serverEnvironment struct {
	Server        string `json:"server"`
	ServerVersion string `json:"server_version"`
}

type serverMetadata struct {
	Environment serverEnvironment `json:"environment"`
	Auth        string            `json:"auth"`
}

type serverEnvelope struct {
	Metadata serverMetadata `json:"metadata"`
}

func parseServerMetadata(data []byte) (serverMetadata, error) {
	var metadata serverMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, fmt.Errorf("decode server metadata: %w", err)
	}
	if metadata.Environment.Server == "" && metadata.Environment.ServerVersion == "" {
		var envelope serverEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			return metadata, fmt.Errorf("decode server metadata: %w", err)
		}
		metadata = envelope.Metadata
	}
	if metadata.Environment.Server == "" || metadata.Environment.ServerVersion == "" {
		return metadata, errors.New("server metadata has no complete environment or version")
	}
	if metadata.Auth != "" && metadata.Auth != "trusted" {
		return metadata, fmt.Errorf("server reports auth %q, not trusted", metadata.Auth)
	}
	return metadata, nil
}

func applyPackages(ctx context.Context, distro distribution, out, errOut io.Writer) error {
	if err := checkExecutable(aptGet); err != nil {
		return fmt.Errorf("the trusted package manager %s is missing or untrusted: %w; this installer requires apt-get from the host distribution and will not use a different installer", aptGet, err)
	}

	fmt.Fprintf(out, "incus install: %s\n", distro.label)
	if distro.caveat != "" {
		fmt.Fprintf(out, "incus install: note: %s\n", distro.caveat)
	}

	fmt.Fprintln(out, "incus install: refreshing package lists with /usr/bin/apt-get update")
	update := runOne(ctx, updateTimeout, maxCapturedOutput, out, errOut, aptGet, updateArgs(), aptEnv())
	if update.err != nil {
		return fmt.Errorf("apt-get update failed: %s; package lists may be partially updated but no packages were installed. %s", operationFailure(update), aptGuidance(update, false))
	}

	fmt.Fprintln(out, "incus install: checking package availability and removals (full bounded simulation)")
	preflight := runOne(ctx, simulateTimeout, maxSimulationOutput, out, errOut, aptGet, simulateArgs(), aptEnv())
	if preflight.truncated {
		return fmt.Errorf("package simulation output exceeded %d bytes; refusing to install because removals and purges could not be fully checked. Package lists were refreshed and no packages were installed", maxSimulationOutput)
	}
	if preflight.err != nil {
		fullOutput := preflight.stdout + "\n" + preflight.stderr
		if removeDisabled(fullOutput) {
			return errors.New("apt refused the package plan because packages would need to be removed and --no-remove is enabled; this is an intentional safety refusal, package lists were refreshed and no packages were installed. The installer will not remove or purge packages")
		}
		if missingRequestedPackage(fullOutput) {
			return fmt.Errorf("%s are not available from the configured %s repositories: %s; package lists were refreshed and no packages were installed. This installer does not add repositories or use an alternative installer; provide a repository that carries them, then re-run. %s", strings.Join(requestedPackages, ", "), distro.label, operationFailure(preflight), aptGuidance(preflight, false))
		}
		return fmt.Errorf("package preflight failed: %s; package lists were refreshed but no packages were installed. %s", operationFailure(preflight), aptGuidance(preflight, false))
	}

	report, err := analyzeSimulation(preflight.stdout, preflight.stderr)
	if err != nil {
		return fmt.Errorf("%w; package lists were refreshed and no packages were installed", err)
	}
	if len(report.installs) > 0 {
		fmt.Fprintf(out, "incus install: simulation package actions: %s\n", strings.Join(report.installs, ", "))
		fmt.Fprintln(out, "incus install: dependency packages may be installed or upgraded from configured repositories; removals and purges abort the install")
	}

	fmt.Fprintf(out, "incus install: installing %s\n", strings.Join(requestedPackages, ", "))
	installed := runOne(ctx, installTimeout, maxCapturedOutput, out, errOut, aptGet, installArgs(), aptEnv())
	if installed.err != nil {
		if removeDisabled(installed.stdout + "\n" + installed.stderr) {
			return errors.New("apt refused the installation because packages would need to be removed and --no-remove is enabled; this is an intentional safety refusal and no packages were installed by this stage. The installer will not remove or purge packages")
		}
		return fmt.Errorf("incus installation did not complete: %s; %s", operationFailure(installed), aptGuidance(installed, true))
	}

	fmt.Fprintf(out, "incus packages installed: %s\n", strings.Join(requestedPackages, " "))
	fmt.Fprintln(out, "incus install: daemon readiness was not verified by this step; distribution maintainer scripts may start or restart Incus")
	printNextSteps(out)
	return nil
}

type simulationReport struct {
	installs []string
	removals []string
}

const maxSimulationLines = 12

func analyzeSimulation(stdout, stderr string) (simulationReport, error) {
	var report simulationReport
	seen := map[string]bool{}
	for _, raw := range strings.Split(stdout+"\n"+stderr, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "Remv"), strings.HasPrefix(line, "Purg"), strings.Contains(line, "REMOVED"):
			report.removals = appendLimited(report.removals, line, maxSimulationLines)
		case strings.HasPrefix(line, "Inst"):
			fields := strings.Fields(line)
			if len(fields) < 2 || seen[fields[1]] {
				continue
			}
			seen[fields[1]] = true
			report.installs = appendLimited(report.installs, fields[1], maxSimulationLines)
		}
	}
	if len(report.removals) > 0 {
		return report, fmt.Errorf("simulation plans package removals or purges: %s; refusing to install because this installer never removes or purges packages", strings.Join(report.removals, "; "))
	}
	return report, nil
}

func appendLimited(list []string, value string, max int) []string {
	if len(list) >= max {
		return list
	}
	return append(list, boundedLine(value, 120))
}

func boundedLine(text string, max int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > max {
		return string(runes[:max]) + "..."
	}
	return text
}

// removeDisabled recognizes apt's intentional --no-remove abort. It aborts
// before running the plan, so it must not be reported as a network, repository,
// package-database or recovery failure.
func removeDisabled(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "need to be removed but remove is disabled") ||
		strings.Contains(lower, "remove is disabled")
}

// missingRequestedPackage reports unavailable candidates only for the exact
// packages this installer requests. A missing or broken unrelated dependency
// stays a generic preflight failure instead of being mislabeled as unsupported.
func missingRequestedPackage(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(line)
		missing := strings.Contains(lower, "no installation candidate") ||
			strings.Contains(lower, "unable to locate package") ||
			strings.Contains(lower, "is not installable") ||
			strings.Contains(lower, "has no candidate")
		if missing && requestedPackagePattern.MatchString(line) {
			return true
		}
	}
	return false
}

func printNextSteps(out io.Writer) {
	fmt.Fprintln(out, "these commands install distribution packages only. Package maintainer scripts may start or restart Incus, dnsmasq or related services and may add subuid/subgid or sysctl entries.")
	fmt.Fprintln(out, "ca-certificates supplies operating-system CA roots for native Incus HTTPS image copy/list; dnsmasq-base supplies the executable needed to create a managed Incus bridge. No repository, URL or TLS setting is changed.")
	fmt.Fprintln(out, "this installer does not initialize Incus (storage pool, managed bridge, cached image), enable a public listener or run project preparation; do those separately, then run `incus-for-devs setup --apply` once those resources exist.")
	fmt.Fprintln(out, "it runs no SSH, firewall, Docker or Tailscale commands.")
}

func aptGuidance(result execResult, interruptedInstall bool) string {
	var lingering *lingeringProcessError
	if errors.As(result.err, &lingering) {
		guidance := fmt.Sprintf("apt-get is still running (pid %d) and was not force-killed; wait until it and any dpkg child have exited before retrying. Do not run recovery commands while the package manager is active", lingering.pid)
		if interruptedInstall {
			guidance += ". After it has exited, run `dpkg --configure -a` and `apt-get -f install`, then re-run this installer"
		}
		return guidance
	}
	if result.canceled {
		if interruptedInstall {
			return "apt-get may still be exiting; wait until apt-get and dpkg have exited before retrying. If the interrupted run left the package database incomplete, run `dpkg --configure -a` and `apt-get -f install` only after it has exited"
		}
		return "apt-get may still be exiting; wait until apt-get and dpkg have exited before retrying"
	}
	if interruptedInstall {
		return "apt or dpkg may be partially applied. After apt-get has exited, run `dpkg --configure -a` and `apt-get -f install`, then re-run this installer"
	}
	return "Fix the package-manager or network error and re-run this installer"
}

func operationFailure(result execResult) string {
	detail := result.detail()
	if detail == "" && result.err != nil {
		detail = result.err.Error()
	}
	if detail == "" {
		detail = "unknown error"
	}
	if result.canceled {
		return "operation timed out or was cancelled: " + detail
	}
	return detail
}

// sanitizedEnv is a minimum allowlist, not a filter of inherited values. It
// drops APT_CONFIG, loader overrides such as LD_* and DYLD_*, proxy/route
// overrides and Incus/LXD remote settings before a root-run package or daemon
// query command starts.
func sanitizedEnv() []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LC_ALL=C",
		"HOME=/root",
	}
}

func aptEnv() []string {
	return append(sanitizedEnv(), "DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none")
}

func incusEnv() []string {
	return sanitizedEnv()
}
