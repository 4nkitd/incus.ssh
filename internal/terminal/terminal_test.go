package terminal

import (
	"testing"

	"incus-for-devs/internal/core"
)

func TestStyleWithoutColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	cases := []struct {
		name string
		s    core.Streams
	}{
		{"no pty", core.Streams{Term: "xterm"}},
		{"empty term", core.Streams{PTY: true}},
		{"dumb term", core.Streams{PTY: true, Term: "dumb"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Style(c.s, Green, "ready"); got != "ready" {
				t.Fatalf("Style() = %q, want plain text", got)
			}
		})
	}
}

func TestStyleNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if got := Style(core.Streams{PTY: true, Term: "xterm-256color"}, Red, "failed"); got != "failed" {
		t.Fatalf("Style() = %q, want plain text with NO_COLOR", got)
	}
}

func TestStyleEmptyArguments(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	s := core.Streams{PTY: true, Term: "xterm"}
	if got := Style(s, "", "text"); got != "text" {
		t.Fatalf("Style() with empty code = %q, want text", got)
	}
	if got := Style(s, Bold, ""); got != "" {
		t.Fatalf("Style() with empty text = %q, want empty", got)
	}
}

func TestStyleWithColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	cases := []struct {
		term string
		want string
	}{
		{"xterm", Cyan + "hello" + Reset},
		{"xterm-256color", Cyan + "hello" + Reset},
		{"screen", Cyan + "hello" + Reset},
	}
	for _, c := range cases {
		t.Run(c.term, func(t *testing.T) {
			if got := Style(core.Streams{PTY: true, Term: c.term}, Cyan, "hello"); got != c.want {
				t.Fatalf("Style() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStyleBytes(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	got := Style(core.Streams{PTY: true, Term: "xterm"}, Yellow, "warn")
	want := "\x1b[33mwarn\x1b[0m"
	if got != want {
		t.Fatalf("Style() = %q, want %q", got, want)
	}
}
