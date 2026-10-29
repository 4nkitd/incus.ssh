package app

import (
	"io"
	"os"
	"testing"
)

func TestVersionLabel(t *testing.T) {
	if Version == "" {
		t.Fatal("version label is empty")
	}
}

func TestVersionCommandOutput(t *testing.T) {
	for _, option := range []string{"version", "--version"} {
		t.Run(option, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			previous := os.Stdout
			os.Stdout = writer
			defer func() { os.Stdout = previous; writer.Close() }()
			runErr := Run([]string{option})
			writer.Close()
			os.Stdout = previous
			data, err := io.ReadAll(reader)
			if runErr != nil || err != nil {
				t.Fatalf("run: %v; output: %v", runErr, err)
			}
			if want := "incus.ssh " + Version + " (incus-for-devs CLI)\n"; string(data) != want {
				t.Fatalf("got %q, want %q", data, want)
			}
		})
	}
}

func TestHelpIsSuccessful(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"server", "--help"}, {"keygen", "--help"}, {"client", "--help"}, {"setup", "--help"}, {"incus", "--help"}} {
		if err := Run(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestClientExplicitEmptyGuestCommand(t *testing.T) {
	args, err := clientArgs([]string{"ssh", "demo", ""}, "example")
	if err != nil {
		t.Fatal(err)
	}
	if got := args[len(args)-1]; got != "'ssh' 'demo' :" {
		t.Fatalf("empty guest command became a shell: %q", got)
	}
}
