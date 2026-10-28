package gateway

import (
	"io"
	"strings"
	"unicode/utf8"
)

const historyLimit = 100

// commandHistory is deliberately in-memory only: history never leaves the session.
type commandHistory struct {
	entries []string
	index   int
	draft   string
}

func newCommandHistory() *commandHistory {
	return &commandHistory{}
}

func (h *commandHistory) add(command string) {
	if command == "" {
		return
	}
	if len(h.entries) == historyLimit {
		copy(h.entries, h.entries[1:])
		h.entries = h.entries[:len(h.entries)-1]
	}
	h.entries = append(h.entries, command)
	h.index = len(h.entries)
	h.draft = ""
}

func (h *commandHistory) reset() {
	h.index = len(h.entries)
	h.draft = ""
}

func (h *commandHistory) up(current []byte) []byte {
	if len(h.entries) == 0 {
		return current
	}
	if h.index == len(h.entries) {
		h.draft = string(current)
	}
	if h.index > 0 {
		h.index--
	}
	return []byte(h.entries[h.index])
}

func (h *commandHistory) down(current []byte) []byte {
	if h.index >= len(h.entries) {
		return current
	}
	h.index++
	if h.index == len(h.entries) {
		return []byte(h.draft)
	}
	return []byte(h.entries[h.index])
}

func (h *commandHistory) edited(line []byte) {
	h.index = len(h.entries)
	h.draft = string(line)
}

// promptEditor owns the management prompt line buffer and its redraw. It reads
// through the REPL reader, so guest stdin never passes through it.
type promptEditor struct {
	history *commandHistory
	out     io.Writer
	prompt  string
	ansi    bool
	line    []byte
	shown   int
}

func newPromptEditor(history *commandHistory, out io.Writer, prompt string, ansi bool) *promptEditor {
	return &promptEditor{history: history, out: out, prompt: prompt, ansi: ansi}
}

func (e *promptEditor) reset() {
	e.line = nil
	e.shown = 0
	e.history.reset()
}

func (e *promptEditor) text() []byte {
	return e.line
}

func (e *promptEditor) length() int {
	return len(e.line)
}

func (e *promptEditor) append(b byte) error {
	e.line = append(e.line, b)
	if e.history.index != len(e.history.entries) {
		e.history.edited(e.line)
	}
	if utf8.RuneStart(b) {
		e.shown++
	}
	if e.out == nil {
		return nil
	}
	_, err := e.out.Write([]byte{b})
	return err
}

func (e *promptEditor) backspace() error {
	if len(e.line) == 0 {
		return nil
	}
	_, width := utf8.DecodeLastRune(e.line)
	e.line = e.line[:len(e.line)-width]
	if e.shown > 0 {
		e.shown--
	}
	if e.history.index != len(e.history.entries) {
		e.history.edited(e.line)
	}
	if e.out == nil {
		return nil
	}
	_, err := io.WriteString(e.out, "\b \b")
	return err
}

func (e *promptEditor) historyMove(up bool) error {
	var next []byte
	if up {
		next = e.history.up(e.line)
	} else {
		next = e.history.down(e.line)
	}
	return e.replace(next)
}

func (e *promptEditor) replace(next []byte) error {
	if e.out != nil {
		var err error
		if e.ansi {
			_, err = io.WriteString(e.out, "\r\x1b[2K"+e.prompt+string(next))
		} else {
			_, err = io.WriteString(e.out, strings.Repeat("\b \b", e.shown)+string(next))
		}
		if err != nil {
			return err
		}
	}
	e.line = next
	e.shown = utf8.RuneCount(next)
	return nil
}

func (e *promptEditor) clear() error {
	if e.out == nil || !e.ansi {
		return nil
	}
	_, err := io.WriteString(e.out, "\x1b[H\x1b[2J"+e.prompt+string(e.line))
	return err
}
