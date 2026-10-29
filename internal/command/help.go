package command

import (
	"fmt"
	"strings"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/terminal"
)

type helpCommand struct {
	Name        string `json:"name"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
}

var commands = []helpCommand{
	{"help", "help [all|COMMAND] [--json]", "Show this command reference. Alias: ?."},
	{"whoami", "whoami [--json]", "Show the authenticated application user and Incus project."},
	{"new", "new [--name=NAME] [--image=REF] [--cpu=N] [--memory=SIZE] [--processes=N] [--disk=SIZE] [--json]", "Create an owned container. Omitted resources default to 1 CPU, 2GiB memory and 256 processes, each capped down by the operator maxima. Creation must also leave a 1GiB host memory reserve. Sizes use an integer and B, kB, KB, MB, GB, TB, KiB, MiB, GiB or TiB. --image selects an administrator-cached image; omitted, the administrator default applies. REF is an alias or fingerprint from image list, optionally prefixed with local:."},
	{"ls", "ls [-l] [--json] [name-or-glob]", "List owned containers. -l adds known CPU, memory, process and disk limits. JSON summaries include the image when known."},
	{"image", "image list [--json]", "List administrator-cached images available to new containers. Read-only: pulling, importing, removing and other image cache changes are administrator work on the host."},
	{"clear", "clear", "Clear the interactive terminal screen. Fails without an interactive terminal."},
	{"rm", "rm [--json] NAME...", "Permanently remove owned containers and their data, without asking for confirmation. Report each failure and any successful removals."},
	{"start", "start NAME [--json]", "Start an owned stopped container; its data is preserved. Alias: resume."},
	{"stop", "stop NAME [--json]", "Gracefully stop an owned container; its data is preserved, running processes are not."},
	{"restart", "restart NAME [--json]", "Gracefully restart an owned container; its data is preserved."},
	{"resume", "resume NAME [--json]", "Alias of start; the container's data is preserved."},
	{"ssh", "ssh NAME [COMMAND...]", "Open an interactive container shell or run one command inside it. Remote options, including --json, belong to the remote command."},
}

const (
	helpWidth              = 78
	unsupportedDescription = "Unsupported: pause (no RAM-to-disk suspend guarantee), share, ssh-key (including list), SFTP/SCP, direct-host routing and port forwarding."
)

var helpParagraphs = []string{
	"incus-for-devs: an exe.dev-compatible management command subset for trusted Incus development containers.",
	"This is not full exe.dev or VM parity. Operator limits are per-container maxima, not defaults, and do not schedule or isolate VMs. Administrator setup runs locally on the host, not through this gateway.",
	"Management flags may follow positional arguments. Instance names use 1-63 lowercase letters, digits or hyphens, start with a letter and end with a letter or digit.",
}

func helpDescription() string {
	return strings.Join(helpParagraphs, " ")
}

func (h Handler) help(r request, s core.Streams) int {
	selected := commands
	if len(r.args) == 1 && r.args[0] != "all" {
		topic := r.args[0]
		if topic == "?" {
			topic = "help"
		}
		for _, command := range commands {
			if command.Name == topic {
				selected = []helpCommand{command}
				break
			}
		}
	}
	if r.json {
		return writeJSON(s, struct {
			Program     string        `json:"program"`
			Description string        `json:"description"`
			Commands    []helpCommand `json:"commands"`
			Unsupported string        `json:"unsupported"`
			Limits      core.Limits   `json:"limits"`
		}{"incus-for-devs", helpDescription(), selected, unsupportedDescription, h.Limits})
	}
	var text strings.Builder
	for i, paragraph := range helpParagraphs {
		if i != 0 {
			text.WriteString("\n")
		}
		text.WriteString(wrapHelp(paragraph, "", "", helpWidth))
		text.WriteString("\n")
	}
	text.WriteString("\n")
	text.WriteString(terminal.Style(s, terminal.Bold, "Commands:"))
	text.WriteString("\n")
	for i, command := range selected {
		if i != 0 {
			text.WriteString("\n")
		}
		text.WriteString(terminal.Style(s, terminal.Cyan, wrapHelp(command.Usage, "  ", "      ", helpWidth)))
		text.WriteString("\n")
		text.WriteString(wrapHelp(command.Description, "    ", "    ", helpWidth))
		text.WriteString("\n")
	}
	text.WriteString("\n")
	text.WriteString(terminal.Style(s, terminal.Yellow, wrapHelp(unsupportedDescription, "", "", helpWidth)))
	text.WriteString("\n")
	if err := writeOutput(s, text.String()); err != nil {
		return failure(s, false, fmt.Errorf("write response: %w", err))
	}
	return 0
}

// wrapHelp wraps space-separated text without splitting a word. Lines stay
// within width columns so help remains readable in a narrow terminal, and a
// wrapped usage keeps its continuation aligned under the command name.
func wrapHelp(text, firstIndent, nextIndent string, width int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	lines := make([]string, 0, len(words)/8+1)
	line := firstIndent + words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = nextIndent + word
			continue
		}
		line += " " + word
	}
	return strings.Join(append(lines, line), "\n")
}
