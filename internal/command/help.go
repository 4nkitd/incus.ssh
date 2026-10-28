package command

import (
	"fmt"

	"incus-for-devs/internal/core"
)

type helpCommand struct {
	Name        string `json:"name"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
}

var commands = []helpCommand{
	{"help", "help [all|COMMAND] [--json]", "Show this exe.dev-compatible subset. Alias: ?."},
	{"whoami", "whoami [--json]", "Show the authenticated application identity and Incus project."},
	{"new", "new [--name=NAME] [--cpu=N] [--memory=SIZE] [--disk=SIZE] [--json]", "Create an owned container using server defaults and resource ceilings. Sizes require an integer and B, kB, KB, MB, GB, TB, KiB, MiB, GiB or TiB."},
	{"ls", "ls [-l] [--json] [name-or-glob]", "List owned containers. -l includes known CPU, memory and disk limits."},
	{"rm", "rm [--json] NAME...", "Remove owned containers; report each failure and any successful removals."},
	{"resume", "resume NAME [--json]", "Start an owned stopped container."},
	{"ssh", "ssh NAME [COMMAND...]", "Open an interactive container shell or execute a command inside it. Remote options, including --json, belong to the command."},
}

const helpDescription = "incus-for-devs: an exe.dev-compatible management subset for trusted Incus development containers. Management flags may follow positional arguments. Instance names use 1-63 lowercase letters, digits or hyphens, start with a letter and end with a letter or digit."
const unsupportedDescription = "Unsupported: pause (no RAM-to-disk suspend guarantee), stop, restart, share, ssh-key (including list), SFTP/SCP, direct-host routing and port forwarding."

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
		}{"incus-for-devs", helpDescription, selected, unsupportedDescription, h.Limits})
	}
	text := helpDescription + "\n\n"
	for _, command := range selected {
		text += fmt.Sprintf("  %s\n    %s\n", command.Usage, command.Description)
	}
	text += "\n" + unsupportedDescription + "\n"
	if err := writeOutput(s, text); err != nil {
		return failure(s, false, fmt.Errorf("write response: %w", err))
	}
	return 0
}
