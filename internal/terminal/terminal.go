// Package terminal styles text for interactive terminal output.
package terminal

import (
	"os"

	"incus-for-devs/internal/core"
)

const (
	Cyan   = "\x1b[36m"
	Green  = "\x1b[32m"
	Yellow = "\x1b[33m"
	Red    = "\x1b[31m"
	Bold   = "\x1b[1m"
	Reset  = "\x1b[0m"
)

// Style wraps text in code only for a genuinely interactive terminal and when
// the server operator has not asked for plain output via NO_COLOR.
func Style(s core.Streams, code, text string) string {
	if code == "" || text == "" || !colored(s) {
		return text
	}
	return code + text + Reset
}

func colored(s core.Streams) bool {
	return s.PTY && s.Term != "" && s.Term != "dumb" && os.Getenv("NO_COLOR") == ""
}
