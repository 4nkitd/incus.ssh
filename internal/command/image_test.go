package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/terminal"
)

var imageFixtures = []core.ImageInfo{
	{
		Fingerprint:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Aliases:      []string{"ubuntu/24.04", "ubuntu-noble"},
		Description:  "Ubuntu 24.04 LTS",
		OS:           "Ubuntu",
		Release:      "24.04",
		Architecture: "x86_64",
	},
	{Fingerprint: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"},
}

func TestImageListJSONMetadataAndEmptyArray(t *testing.T) {
	f := &fakeBackend{images: imageFixtures}
	status, out, stderr := run(Handler{Backend: f}, "image list --json")
	if status != 0 || stderr != "" || len(f.calls) != 1 || f.calls[0].method != "images" {
		t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
	}
	want := map[string]any{"images": []any{
		map[string]any{
			"fingerprint":  imageFixtures[0].Fingerprint,
			"aliases":      []any{"ubuntu/24.04", "ubuntu-noble"},
			"description":  "Ubuntu 24.04 LTS",
			"os":           "Ubuntu",
			"release":      "24.04",
			"architecture": "x86_64",
		},
		map[string]any{
			"fingerprint": imageFixtures[1].Fingerprint,
			"aliases":     []any{},
		},
	}}
	if got := decodeObject(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("images=%v want=%v", got, want)
	}

	for _, images := range [][]core.ImageInfo{nil, {}} {
		status, out, stderr := run(Handler{Backend: &fakeBackend{images: images}}, "image list --json")
		if status != 0 || stderr != "" || out != "{\"images\":[]}\n" {
			t.Fatalf("empty image list: status=%d out=%q stderr=%q", status, out, stderr)
		}
	}
}

func TestImageListHumanOutput(t *testing.T) {
	f := &fakeBackend{images: []core.ImageInfo{
		imageFixtures[0],
		{Fingerprint: "fedcba9876543210", OS: "Alpine", Release: "3.20"},
		{Fingerprint: "ab"},
	}}
	status, out, stderr := run(Handler{Backend: f}, "image list")
	want := "ubuntu/24.04\t0123456789ab\tUbuntu 24.04\n" +
		"fedcba987654\tfedcba987654\tAlpine 3.20\n" +
		"ab\tab\n"
	if status != 0 || stderr != "" || out != want {
		t.Fatalf("status=%d out=%q stderr=%q want=%q", status, out, stderr, want)
	}
}

func TestImageListHumanColorOnlyOnPTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	streams, out, errOut := colorStreams()
	h := Handler{Backend: &fakeBackend{images: imageFixtures[:1]}}
	if status := h.Handle(context.Background(), principal, "image list", streams); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	want := terminal.Style(streams, terminal.Cyan, "ubuntu/24.04") + "\t0123456789ab\tUbuntu 24.04\r\n"
	if out.String() != want {
		t.Fatalf("colored image list=%q want=%q", out.String(), want)
	}
	assertCRLF(t, out.String())

	out.Reset()
	errOut.Reset()
	plain := core.Streams{Out: out, Err: errOut}
	if status := (Handler{Backend: &fakeBackend{images: imageFixtures[:1]}}).Handle(context.Background(), principal, "image list", plain); status != 0 {
		t.Fatalf("plain status=%d stderr=%q", status, errOut.String())
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("non-PTY image list contains ANSI: %q", out.String())
	}
}

func TestImageListFlagPositions(t *testing.T) {
	for _, line := range []string{"image list --json", "image --json list"} {
		f := &fakeBackend{images: imageFixtures}
		status, out, stderr := run(Handler{Backend: f}, line)
		if status != 0 || stderr != "" || len(f.calls) != 1 || f.calls[0].method != "images" {
			t.Fatalf("%s: status=%d out=%q stderr=%q calls=%v", line, status, out, stderr, f.calls)
		}
		if !strings.HasPrefix(out, "{\"images\":") {
			t.Fatalf("%s is not JSON: %q", line, out)
		}
	}
}

func TestImageMutationsAndRemotesAreRejectedBeforeBackend(t *testing.T) {
	for _, line := range []string{
		"image", "image list extra", "image list --json=true", "image list --remote=images:ubuntu",
		"image pull ubuntu/24.04", "image pull --json ubuntu", "image add ubuntu", "image remove ubuntu",
		"image rm ubuntu", "image import image.tar", "image export ubuntu", "image copy ubuntu", "image alias list",
	} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{}
			status, out, stderr := run(Handler{Backend: f}, line)
			if status == 0 || len(f.calls) != 0 || out+stderr == "" {
				t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
			}
			if strings.HasPrefix(line, "image pull") || strings.HasPrefix(line, "image add") || strings.HasPrefix(line, "image remove") {
				if !strings.Contains(out+stderr, "unsupported") {
					t.Fatalf("%s did not explain the rejected mutation: %q %q", line, out, stderr)
				}
			}
		})
	}
}

func TestImageListBackendError(t *testing.T) {
	err := errors.New("image metadata unavailable")
	for _, line := range []string{"image list", "image list --json"} {
		f := &fakeBackend{imagesError: err}
		status, out, stderr := run(Handler{Backend: f}, line)
		if status == 0 || len(f.calls) != 1 || !strings.Contains(out+stderr, err.Error()) {
			t.Fatalf("%s: status=%d out=%q stderr=%q calls=%v", line, status, out, stderr, f.calls)
		}
		if strings.HasSuffix(line, "--json") {
			if decodeObject(t, out)["error"] != err.Error() || stderr != "" {
				t.Fatalf("%s: JSON error out=%q stderr=%q", line, out, stderr)
			}
		} else if out != "" || stderr == "" {
			t.Fatalf("%s: text error out=%q stderr=%q", line, out, stderr)
		}
	}
}

func TestNewImagePassedUnchangedWithCombinedOptions(t *testing.T) {
	for _, ref := range []string{
		"ubuntu",
		"ubuntu/24.04",
		"local:ubuntu/24.04",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"a-c.d/e",
	} {
		t.Run(ref, func(t *testing.T) {
			f := &fakeBackend{created: core.Instance{Name: "work", Status: "Running", Image: ref}}
			line := "new --name=work --image=" + ref + " --cpu=2 --memory=512MiB --processes=8 --disk=10GB --json"
			status, out, stderr := run(Handler{Backend: f}, line)
			want := core.CreateOptions{Name: "work", Image: ref, CPU: 2, Memory: "512MiB", Processes: 8, Disk: "10GB"}
			if status != 0 || stderr != "" || f.options != want {
				t.Fatalf("status=%d out=%q stderr=%q options=%+v want=%+v", status, out, stderr, f.options, want)
			}
			got := decodeObject(t, out)
			if got["image"] != ref || got["vm_name"] != "work" {
				t.Fatalf("new summary=%v", got)
			}
		})
	}

	f := &fakeBackend{}
	status, _, stderr := run(Handler{Backend: f}, "new")
	if status != 0 || stderr != "" || f.options.Image != "" {
		t.Fatalf("default image changed: status=%d stderr=%q options=%+v", status, stderr, f.options)
	}
}

func TestNewRejectsInvalidImageReferences(t *testing.T) {
	for _, line := range []string{
		"new --image=",
		"new --image",
		"new --image=--latest",
		"new --image=-ubuntu",
		"new --image=http://example.com/ubuntu",
		"new --image=images:ubuntu",
		"new --image=ubuntu:24.04",
		"new --image=ubuntu/../x",
		"new --image=ubuntu/./x",
		"new --image=ubuntu//x",
		"new --image=ubuntu/",
		"new --image=/ubuntu",
		"new --image=ubuntu@24.04",
		"new --image=local:",
		"new --image=local:../x",
		"new --image='ubuntu 24.04'",
		"new --image=" + strings.Repeat("a", maxImageRefBytes+1),
		"new --image=ubuntu --image=debian",
	} {
		t.Run(line, func(t *testing.T) {
			f := &fakeBackend{}
			status, out, stderr := run(Handler{Backend: f}, line)
			if status == 0 || len(f.calls) != 0 || out+stderr == "" {
				t.Fatalf("status=%d out=%q stderr=%q calls=%v", status, out, stderr, f.calls)
			}
		})
	}
}

func TestInstanceSummaryCarriesImageWithoutChangingLongList(t *testing.T) {
	f := &fakeBackend{instances: []core.Instance{
		{Name: "work", Status: "Running", CPU: "2", Memory: "1GiB", Processes: "512", Disk: "10GB", Image: "ubuntu/24.04"},
		{Name: "plain", Status: "Stopped"},
	}}
	_, out, _ := run(Handler{Backend: f}, "ls --json")
	want := map[string]any{"vms": []any{
		map[string]any{"vm_name": "work", "status": "Running", "cpu": "2", "memory": "1GiB", "processes": "512", "disk": "10GB", "image": "ubuntu/24.04"},
		map[string]any{"vm_name": "plain", "status": "Stopped"},
	}}
	if got := decodeObject(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("ls summary=%v want=%v", got, want)
	}

	_, out, _ = run(Handler{Backend: f}, "ls -l")
	if out != "work\tRunning\t2\t1GiB\t512\t10GB\nplain\tStopped\t\t\t\t\n" {
		t.Fatalf("long list changed by image: %q", out)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.Contains(line, "ubuntu") {
			t.Fatalf("long list gained an image column: %q", line)
		}
	}
}

func TestImageListKeepsContextAndPrincipal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeBackend{}
	if status := (Handler{Backend: f}).Handle(ctx, principal, "image list", core.Streams{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); status != 0 {
		t.Fatalf("status=%d", status)
	}
	if len(f.calls) != 1 || f.calls[0].ctx != ctx || f.calls[0].principal != principal {
		t.Fatalf("lost context/principal: %+v", f.calls)
	}
}

func TestImageListJSONHasNoANSIOnPTY(t *testing.T) {
	streams, out, errOut := ptyStreams()
	h := Handler{Backend: &fakeBackend{images: imageFixtures}}
	if status := h.Handle(context.Background(), principal, "image list --json", streams); status != 0 {
		t.Fatalf("status=%d stderr=%q", status, errOut.String())
	}
	if strings.Contains(out.String(), "\x1b") || strings.Contains(errOut.String(), "\x1b") {
		t.Fatalf("image JSON emitted ANSI: out=%q stderr=%q", out.String(), errOut.String())
	}
	assertCRLF(t, out.String())
	var decoded struct {
		Images []core.ImageInfo `json:"images"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(decoded.Images) != len(imageFixtures) {
		t.Fatalf("images=%v", decoded.Images)
	}
}
