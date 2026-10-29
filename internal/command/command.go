package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/shlex"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/terminal"
)

const maxLineBytes = 16 * 1024
const maxArguments = 256

// An explicitly empty remote command runs the shell no-op so it stays distinct
// from an absent command, which opens an interactive login shell and reads stdin.
const noopRemoteCommand = ":"

type Handler struct {
	Backend core.Backend
	Limits  core.Limits
	Sharing core.Sharing
	Files   core.FileBackend
}

var _ core.Handler = (*Handler)(nil)

var namePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
var sizePattern = regexp.MustCompile(`^([0-9]+)(B|kB|KB|MB|GB|TB|KiB|MiB|GiB|TiB)$`)
var filterPattern = regexp.MustCompile(`^[a-zA-Z0-9*?\[\]!^-]+$`)

// An image reference is a cached alias or fingerprint. Only the local: remote
// prefix is accepted, and path segments may not traverse.
var imageRefPattern = regexp.MustCompile(`^(?:local:)?[A-Za-z0-9][A-Za-z0-9._/-]*$`)

const maxImageRefBytes = 256

type request struct {
	command string
	json    bool
	long    bool
	args    []string
	create  core.CreateOptions
	remote  string
}

type instanceSummary struct {
	Name      string `json:"vm_name"`
	Status    string `json:"status"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Processes string `json:"processes,omitempty"`
	Disk      string `json:"disk,omitempty"`
	Image     string `json:"image,omitempty"`
}

func summary(vm core.Instance) instanceSummary {
	return instanceSummary{Name: vm.Name, Status: vm.Status, CPU: vm.CPU, Memory: vm.Memory, Processes: vm.Processes, Disk: vm.Disk, Image: vm.Image}
}

func (h Handler) Handle(ctx context.Context, p core.Principal, line string, s core.Streams) int {
	execStreams := s
	if s.Out == nil {
		s.Out = io.Discard
	}
	if s.Err == nil {
		s.Err = io.Discard
	}
	r, err := parse(line)
	if err != nil {
		return failure(s, r.json, err)
	}
	switch r.command {
	case "help", "?":
		return h.help(r, s)
	case "whoami":
		if r.json {
			return writeJSON(s, p)
		}
		return writeText(s, "%s\t%s\n", p.Name, p.Project)
	case "clear":
		return clearScreen(s)
	case "share":
		return h.share(ctx, p, r, s)
	}
	if h.Backend == nil {
		return failure(s, r.json, errors.New("backend is not configured"))
	}
	switch r.command {
	case "new":
		vm, err := h.Backend.Create(ctx, p, r.create)
		if err != nil {
			return failure(s, r.json, err)
		}
		if r.json {
			return writeJSON(s, summary(vm))
		}
		return writeText(s, "%s\t%s\n", vm.Name, statusStyle(s, vm.Status))
	case "ls":
		vms, err := h.Backend.List(ctx, p)
		if err != nil {
			return failure(s, r.json, err)
		}
		items := make([]instanceSummary, 0, len(vms))
		for _, vm := range vms {
			if len(r.args) != 0 {
				matched, _ := path.Match(r.args[0], vm.Name)
				if !matched {
					continue
				}
			}
			items = append(items, summary(vm))
		}
		if r.json {
			return writeJSON(s, struct {
				VMs []instanceSummary `json:"vms"`
			}{items})
		}
		for _, vm := range items {
			name := terminal.Style(s, terminal.Bold+terminal.Cyan, vm.Name)
			state := statusStyle(s, vm.Status)
			var status int
			if r.long {
				status = writeText(s, "%s\t%s\t%s\t%s\t%s\t%s\n", name, state, vm.CPU, vm.Memory, vm.Processes, vm.Disk)
			} else {
				status = writeText(s, "%s\t%s\n", name, state)
			}
			if status != 0 {
				return status
			}
		}
		return 0
	case "rm":
		return h.remove(ctx, p, r, s)
	case "image":
		return h.imageList(ctx, p, r, s)
	case "resume":
		return h.lifecycle(ctx, p, r, s, "resumed", h.Backend.Resume)
	case "start":
		return h.lifecycle(ctx, p, r, s, "started", h.Backend.Start)
	case "stop":
		return h.lifecycle(ctx, p, r, s, "stopped", h.Backend.Stop)
	case "restart":
		return h.lifecycle(ctx, p, r, s, "restarted", h.Backend.Restart)
	case "ssh":
		status, err := h.Backend.Exec(ctx, p, r.args[0], r.remote, execStreams)
		if err != nil {
			return failure(s, false, err)
		}
		return status
	}
	return failure(s, r.json, errors.New("unsupported command"))
}

// imageList is read-only: image cache mutations stay administrator work on the
// host, so no image name or remote option is ever forwarded to Incus.
func (h Handler) imageList(ctx context.Context, p core.Principal, r request, s core.Streams) int {
	infos, err := h.Backend.Images(ctx, p)
	if err != nil {
		return failure(s, r.json, err)
	}
	items := make([]core.ImageInfo, 0, len(infos))
	for _, info := range infos {
		if info.Aliases == nil {
			info.Aliases = []string{}
		}
		items = append(items, info)
	}
	if r.json {
		return writeJSON(s, struct {
			Images []core.ImageInfo `json:"images"`
		}{items})
	}
	for _, info := range items {
		plainName := imageName(info)
		name := terminal.Style(s, terminal.Cyan, plainName)
		fields := []string{name, shortFingerprint(info.Fingerprint)}
		if osRelease := imageOSRelease(info); osRelease != "" {
			fields = append(fields, osRelease)
		}
		if status := writeText(s, "%s\n", strings.Join(fields, "\t")); status != 0 {
			return status
		}
	}
	return 0
}

func imageName(info core.ImageInfo) string {
	for _, alias := range info.Aliases {
		if alias != "" {
			return alias
		}
	}
	return shortFingerprint(info.Fingerprint)
}

func shortFingerprint(fingerprint string) string {
	if len(fingerprint) > 12 {
		return fingerprint[:12]
	}
	return fingerprint
}

func imageOSRelease(info core.ImageInfo) string {
	parts := make([]string, 0, 2)
	for _, part := range []string{info.OS, info.Release} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}

func (h Handler) remove(ctx context.Context, p core.Principal, r request, s core.Streams) int {
	result := struct {
		Removed []string          `json:"removed"`
		Errors  map[string]string `json:"errors,omitempty"`
		Error   string            `json:"error,omitempty"`
	}{Removed: make([]string, 0, len(r.args))}
	status := 0
	for _, name := range r.args {
		if err := h.machineChange(ctx, p, name, func() error { return h.Backend.Remove(ctx, p, name) }); err != nil {
			status = 1
			if result.Errors == nil {
				result.Errors = make(map[string]string)
			}
			result.Errors[name] = err.Error()
			if !r.json {
				failure(s, false, fmt.Errorf("%s: %w", name, err))
			}
			continue
		}
		result.Removed = append(result.Removed, name)
		if !r.json && writeText(s, "%s\n", terminal.Style(s, terminal.Yellow, "removed "+name)) != 0 {
			status = 1
		}
	}
	if r.json {
		if status != 0 {
			result.Error = "one or more instances could not be removed"
		}
		if writeJSON(s, result) != 0 {
			return 1
		}
	}
	return status
}

func (h Handler) lifecycle(ctx context.Context, p core.Principal, r request, s core.Streams, action string, apply func(context.Context, core.Principal, string) error) int {
	name := r.args[0]
	if err := h.machineChange(ctx, p, name, func() error { return apply(ctx, p, name) }); err != nil {
		return failure(s, r.json, err)
	}
	if r.json {
		return writeJSON(s, struct {
			Name string `json:"vm_name"`
		}{name})
	}
	if action == "stopped" {
		return writeText(s, "%s %s\n", terminal.Style(s, terminal.Yellow, action), name)
	}
	return writeText(s, "%s %s\n", terminal.Style(s, terminal.Green, action), name)
}

func (h Handler) machineChange(ctx context.Context, p core.Principal, name string, apply func() error) error {
	if h.Sharing != nil {
		return h.Sharing.ChangeMachine(ctx, p, name, apply)
	}
	return apply()
}

// statusStyle keeps running green and every other reported status amber; it
// never maps or invents a status of its own.
func statusStyle(s core.Streams, status string) string {
	if status == "Running" {
		return terminal.Style(s, terminal.Green, status)
	}
	return terminal.Style(s, terminal.Yellow, status)
}

// The clear sequence is written verbatim: screen clearing must not grow CRLF
// pairs or be suppressed by NO_COLOR, which only governs color.
const clearSequence = "\x1b[2J\x1b[H"

func clearScreen(s core.Streams) int {
	if !s.PTY || s.Term == "" || s.Term == "dumb" {
		return failure(s, false, errors.New("clear requires an interactive terminal"))
	}
	if err := writeOutputTo(s.Out, false, clearSequence); err != nil {
		return failure(s, false, fmt.Errorf("write response: %w", err))
	}
	return 0
}

func parse(line string) (request, error) {
	r := request{json: requestedJSON(line)}
	if len(line) > maxLineBytes {
		return r, fmt.Errorf("command exceeds %d bytes", maxLineBytes)
	}
	if !utf8.ValidString(line) {
		return r, errors.New("command must be valid UTF-8")
	}
	for _, ch := range line {
		if unicode.IsControl(ch) && ch != '\t' {
			return r, errors.New("command contains a control character")
		}
	}
	word, tail, err := firstWord(line)
	if err != nil {
		return r, err
	}
	if word == "" {
		return r, errors.New("missing command; use help")
	}
	r.command = word
	if word == "ssh" {
		name, remote, err := firstWord(tail)
		if err != nil {
			return r, err
		}
		if !namePattern.MatchString(name) {
			return r, errors.New("ssh requires a valid instance name before the remote command")
		}
		r.args = []string{name}
		r.remote = remoteCommand(remote)
		return r, nil
	}
	tokens, err := shlex.Split(tail)
	if err != nil {
		return r, errors.New("invalid command quoting")
	}
	if len(tokens) > maxArguments {
		return r, fmt.Errorf("too many arguments; maximum is %d", maxArguments)
	}
	if err := unsupported(word); err != nil {
		return r, err
	}
	seen := make(map[string]bool)
	for _, token := range tokens {
		if !strings.HasPrefix(token, "-") {
			r.args = append(r.args, token)
			continue
		}
		flag, value, hasValue := strings.Cut(token, "=")
		if seen[flag] {
			return r, fmt.Errorf("duplicate flag %s", flag)
		}
		seen[flag] = true
		switch {
		case flag == "--json" && !hasValue && word != "clear":
		case word == "ls" && flag == "-l" && !hasValue:
			r.long = true
		case word == "new" && hasValue:
			switch flag {
			case "--name":
				if !namePattern.MatchString(value) {
					return r, invalidName(value)
				}
				r.create.Name = value
			case "--cpu", "--processes":
				n, err := positiveInteger(value)
				if err != nil {
					return r, fmt.Errorf("%s requires a positive integer: %w", flag, err)
				}
				if flag == "--cpu" {
					r.create.CPU = n
				} else {
					r.create.Processes = n
				}
			case "--memory", "--disk":
				if err := validSize(value); err != nil {
					return r, fmt.Errorf("%s: %w", flag, err)
				}
				if flag == "--memory" {
					r.create.Memory = value
				} else {
					r.create.Disk = value
				}
			case "--image":
				if err := validImageRef(value); err != nil {
					return r, fmt.Errorf("--image: %w", err)
				}
				r.create.Image = value
			default:
				return r, fmt.Errorf("unknown flag %s for %s", flag, word)
			}
		default:
			return r, fmt.Errorf("unknown or invalid flag %s for %s", token, word)
		}
	}
	switch word {
	case "new", "whoami", "clear":
		if len(r.args) != 0 {
			return r, fmt.Errorf("%s does not accept positional arguments", word)
		}
	case "help", "?":
		if len(r.args) > 1 {
			return r, errors.New("usage: help [all|COMMAND] [--json]")
		}
		if len(r.args) == 1 && r.args[0] != "all" {
			if err := unsupported(r.args[0]); err != nil {
				return r, err
			}
		}
	case "ls":
		if len(r.args) > 1 {
			return r, errors.New("usage: ls [-l] [--json] [name-or-glob]")
		}
		if len(r.args) == 1 {
			filter := r.args[0]
			if len(filter) > 128 || !filterPattern.MatchString(filter) {
				return r, errors.New("invalid instance name or glob")
			}
			if _, err := path.Match(filter, ""); err != nil {
				return r, fmt.Errorf("invalid glob: %w", err)
			}
		}
	case "rm", "resume", "start", "stop", "restart":
		if word == "rm" && len(r.args) == 0 {
			return r, errors.New("rm requires one or more names")
		}
		if word != "rm" && len(r.args) != 1 {
			return r, fmt.Errorf("%s requires exactly one name", word)
		}
		names := make(map[string]bool)
		for _, name := range r.args {
			if !namePattern.MatchString(name) {
				return r, invalidName(name)
			}
			if names[name] {
				return r, fmt.Errorf("duplicate instance name %q", name)
			}
			names[name] = true
		}
	case "image":
		if len(r.args) == 0 {
			return r, errors.New("usage: image list [--json]")
		}
		switch r.args[0] {
		case "list":
			if len(r.args) != 1 {
				return r, errors.New("usage: image list [--json]")
			}
		case "add", "remove", "rm", "delete", "import", "export", "copy", "pull", "push", "refresh", "alias", "set", "unset", "edit":
			return r, fmt.Errorf("image %s is unsupported: only image list is available", r.args[0])
		default:
			return r, fmt.Errorf("unknown image subcommand %q; use image list", r.args[0])
		}
	case "share":
		if err := validateShareArgs(r.args); err != nil {
			return r, err
		}
	}
	return r, nil
}

// Identify JSON mode even for invalid management input, without inspecting an
// SSH command tail or scanning beyond the command-size bound.
func requestedJSON(line string) bool {
	if len(line) > maxLineBytes {
		line = line[:maxLineBytes]
	}
	command, rest, err := firstWord(line)
	if err != nil || command == "ssh" {
		return false
	}
	if tokens, err := shlex.Split(rest); err == nil {
		for _, token := range tokens {
			if token == "--json" {
				return true
			}
		}
		return false
	}
	for rest != "" {
		var token string
		token, rest, err = firstWord(rest)
		if err != nil {
			return false
		}
		if token == "--json" {
			return true
		}
	}
	return false
}

func invalidName(name string) error {
	return fmt.Errorf("invalid instance name %q; use 1-63 lowercase letters, digits or hyphens, starting with a letter and ending with a letter or digit", name)
}

func positiveInteger(value string) (int, error) {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, errors.New("expected decimal digits")
	}
	n, err := strconv.ParseUint(value, 10, 31)
	if err != nil || n == 0 {
		return 0, errors.New("integer must be between 1 and 2147483647")
	}
	return int(n), nil
}

func validSize(value string) error {
	parts := sizePattern.FindStringSubmatch(value)
	if parts == nil {
		return errors.New("expected a positive integer size with a unit, such as 512MiB or 2GB")
	}
	n, err := strconv.ParseUint(parts[1], 10, 63)
	if err != nil || n == 0 {
		return errors.New("size must be a positive 63-bit integer")
	}
	return nil
}

// validImageRef accepts a cached alias or fingerprint without resolving it.
// The backend owns authoritative lookup, so this only rejects input that can
// never be a cache reference: control bytes, URLs, unsupported remote prefixes
// and path traversal.
func validImageRef(value string) error {
	if value == "" {
		return errors.New("image reference must not be empty")
	}
	if len(value) > maxImageRefBytes {
		return fmt.Errorf("image reference exceeds %d bytes", maxImageRefBytes)
	}
	for _, ch := range value {
		if ch > unicode.MaxASCII {
			return errors.New("image reference must be ASCII")
		}
		if unicode.IsControl(ch) {
			return errors.New("image reference must not contain control characters")
		}
	}
	if strings.Contains(value, "://") {
		return errors.New("image reference must be a cached alias or fingerprint, not a URL")
	}
	if !imageRefPattern.MatchString(value) {
		return errors.New("invalid image reference; use an alias or fingerprint from image list, optionally prefixed with local:")
	}
	ref := strings.TrimPrefix(value, "local:")
	for _, segment := range strings.Split(ref, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("image reference must not contain empty, . or .. path segments")
		}
	}
	return nil
}

func unsupported(command string) error {
	switch command {
	case "help", "?", "whoami", "new", "ls", "image", "share", "clear", "rm", "resume", "start", "stop", "restart", "ssh":
		return nil
	case "pause":
		return errors.New("pause is unsupported: Incus containers cannot promise exe.dev RAM-to-disk suspend; stop and freeze are not substitutes")
	case "ssh-key", "sftp", "scp", "portforward", "port-forward", "forward", "direct-host":
		return fmt.Errorf("%s is unsupported by incus-for-devs", command)
	default:
		return fmt.Errorf("unknown command %q; use help", command)
	}
}

// firstWord locates the raw SSH tail before shlex can discard quotes or operators.
func firstWord(line string) (string, string, error) {
	line = strings.TrimLeft(line, " \t")
	if line == "" {
		return "", "", nil
	}
	quote := byte(0)
	escaped := false
	end := len(line)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
		} else if ch == ' ' || ch == '\t' {
			end = i
			break
		}
	}
	if quote != 0 || escaped {
		return "", "", errors.New("invalid command quoting")
	}
	tokens, err := shlex.Split(line[:end])
	if err != nil || len(tokens) != 1 {
		return "", "", errors.New("invalid command token")
	}
	return tokens[0], strings.TrimLeft(line[end:], " \t"), nil
}

// A single fully quoted argument is a command envelope. Otherwise the tail is
// passed byte-for-byte to the container shell, never to a host shell. An
// explicitly empty envelope becomes the shell no-op; an absent tail stays empty
// so the backend opens an interactive shell only when no command was supplied.
func remoteCommand(raw string) string {
	trimmed := strings.TrimRight(raw, " \t")
	if len(trimmed) < 2 || (trimmed[0] != '\'' && trimmed[0] != '"') {
		return raw
	}
	quote := trimmed[0]
	var command strings.Builder
	for i := 1; i < len(trimmed); i++ {
		ch := trimmed[i]
		if ch == quote {
			if i == len(trimmed)-1 {
				if command.Len() == 0 {
					return noopRemoteCommand
				}
				return command.String()
			}
			return raw
		}
		if quote == '"' && ch == '\\' && i+1 < len(trimmed) {
			next := trimmed[i+1]
			if next == '$' || next == '`' || next == '"' || next == '\\' {
				command.WriteByte(next)
				i++
				continue
			}
		}
		command.WriteByte(ch)
	}
	return raw
}

func failure(s core.Streams, asJSON bool, err error) int {
	if asJSON {
		data, encodeErr := json.Marshal(struct {
			Error string `json:"error"`
		}{err.Error()})
		if encodeErr == nil {
			encodeErr = writeOutput(s, string(data)+"\n")
		}
		if encodeErr != nil {
			_ = writeOutputTo(s.Err, s.PTY, fmt.Sprintf("error: write response: %v\n", encodeErr))
		}
	} else {
		if writeErr := writeOutputTo(s.Err, s.PTY, fmt.Sprintf("error: %s\n", terminal.Style(s, terminal.Red, err.Error()))); writeErr != nil {
			return 1
		}
	}
	return 1
}

func writeJSON(s core.Streams, value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		return failure(s, false, fmt.Errorf("write response: %w", err))
	}
	if err := writeOutput(s, string(data)+"\n"); err != nil {
		return failure(s, false, fmt.Errorf("write response: %w", err))
	}
	return 0
}

func writeOutput(s core.Streams, text string) error {
	return writeOutputTo(s.Out, s.PTY, text)
}

// A PTY expects CRLF line endings, while management output is built with LF.
func writeOutputTo(w io.Writer, pty bool, text string) error {
	if w == nil {
		return nil
	}
	if pty {
		text = strings.ReplaceAll(text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\n", "\r\n")
	}
	_, err := io.WriteString(w, text)
	return err
}

func writeText(s core.Streams, format string, values ...any) int {
	if err := writeOutput(s, fmt.Sprintf(format, values...)); err != nil {
		return failure(s, false, fmt.Errorf("write response: %w", err))
	}
	return 0
}
