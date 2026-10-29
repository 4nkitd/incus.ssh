package incus

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestSFTPProcessIsOwnedAndContainerOnly(t *testing.T) {
	payload := []byte("\x00\xffSFTP bytes\n")
	f := newFixture(t, []instance{ownedInstance("demo", "alice")}, scenario{Stdout: payload})
	process, err := f.backend.OpenSFTP(context.Background(), alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	got, err := io.ReadAll(process)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("SFTP subprocess bytes changed", err)
	}
	calls := f.calls(t)
	args := calls[len(calls)-1]
	if args[4] != "exec" || args[5] != "demo" || !strings.Contains(strings.Join(args, " "), sftpCommand) {
		t.Fatal("SFTP did not run solely in the selected container", args)
	}
	foreign := ownedInstance("foreign", "bob")
	stopped := ownedInstance("stopped", "alice")
	stopped.Status = "Stopped"
	writeJSON(t, filepath.Join(f.dir, "instances.json"), []instance{foreign, stopped})
	for _, name := range []string{"foreign", "stopped", "--project=default", "../host"} {
		if process, err := f.backend.OpenSFTP(context.Background(), alice, name); err == nil {
			process.Close()
			t.Fatal("accepted invalid file transfer scope", name)
		}
	}
}
