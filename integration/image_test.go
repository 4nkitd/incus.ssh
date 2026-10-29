package integration_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
)

// These are fake unit fixtures standing in for the adapter's cached operator
// catalog, not a model of the real backend. Fingerprints are synthetic 64-hex
// strings so the tests need no Incus host and expose no production metadata.
const (
	ubuntuImageFingerprint = "a3b1c9d7e5f01324a3b1c9d7e5f01324a3b1c9d7e5f01324a3b1c9d7e5f01324"
	debianImageFingerprint = "7f2e4d6c8b0a19357f2e4d6c8b0a19357f2e4d6c8b0a19357f2e4d6c8b0a1935"
)

var imageFixtures = []core.ImageInfo{
	{
		Fingerprint:  ubuntuImageFingerprint,
		Aliases:      []string{"ubuntu-24.04", "ubuntu/noble"},
		Description:  "Ubuntu 24.04 LTS",
		OS:           "ubuntu",
		Release:      "24.04",
		Architecture: "x86_64",
	},
	{
		Fingerprint:  debianImageFingerprint,
		Aliases:      []string{"debian-12"},
		Description:  "Debian 12",
		OS:           "debian",
		Release:      "12",
		Architecture: "x86_64",
	},
}

// canonicalImages mirrors the real adapter's ordering: fingerprints ascending
// and aliases sorted within each image. Tests compare against this so the fake
// wire order is not an accidental assumption.
func canonicalImages(images []core.ImageInfo) []core.ImageInfo {
	result := make([]core.ImageInfo, 0, len(images))
	for _, info := range images {
		info.Aliases = append([]string(nil), info.Aliases...)
		sort.Strings(info.Aliases)
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Fingerprint < result[j].Fingerprint })
	return result
}

var imageCatalog = canonicalImages(imageFixtures)

// fixtureImage resolves an alias or fingerprint against the fake catalog the
// way the request layer expects. Remote, OCI and VM references never resolve.
func fixtureImage(reference string) (core.ImageInfo, bool) {
	ref := strings.TrimPrefix(reference, "local:")
	if ref == "" || strings.Contains(ref, ":") {
		return core.ImageInfo{}, false
	}
	for _, info := range imageCatalog {
		if strings.EqualFold(info.Fingerprint, ref) {
			return info, true
		}
		for _, alias := range info.Aliases {
			if alias == ref {
				return info, true
			}
		}
	}
	return core.ImageInfo{}, false
}

func TestSSHImageCatalogWire(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)
	other := s.connect(t, bob)

	before := s.backend.callCount()
	assertImageCatalog(t, run(t, client, "image list --json", nil, 0))
	if got := s.backend.callCount(); got != before+1 {
		t.Fatalf("image list backend calls = %d, want %d", got, before+1)
	}
	// The fixture catalog stands in for an operator-managed cache, so every
	// authenticated user sees the same read-only view.
	assertImageCatalog(t, run(t, other, "image list --json", nil, 0))

	human := run(t, client, "image list", nil, 0)
	assertNoANSI(t, human.out)
	// Canonical fixture order is by fingerprint, so Debian (7f...) precedes
	// Ubuntu (a3...); aliases are sorted within each image.
	wantLines := []string{
		"debian-12\t7f2e4d6c8b0a\tdebian 12\n",
		"ubuntu-24.04\ta3b1c9d7e5f0\tubuntu 24.04\n",
	}
	offset := 0
	for _, line := range wantLines {
		at := strings.Index(human.out[offset:], line)
		if at < 0 {
			t.Fatalf("plain image list omits %q in order: %q", line, human.out)
		}
		offset += at + len(line)
	}
	// A guest exec here would mean the wire routed a listing through container
	// execution. Host-side Incus argv safety is covered by adapter unit tests.
	if got := s.backend.lastExec(); got.command != "" {
		t.Fatalf("image list issued a guest exec: %q", got.command)
	}

	pty := runPTY(t, client, "image list", nil, 0)
	assertPTYLineEndings(t, pty.out)
	if !strings.Contains(pty.out, ansiCyan+"ubuntu-24.04"+ansiReset) || !strings.Contains(pty.out, ansiCyan+"debian-12"+ansiReset) {
		t.Fatalf("PTY image list lacks styled aliases: %q", pty.out)
	}

	var raw struct {
		Images []core.ImageInfo `json:"images"`
	}
	assertRawJSON(t, runPTY(t, client, "image list --json", nil, 0).out, &raw)
	if !reflect.DeepEqual(raw.Images, imageCatalog) {
		t.Fatalf("PTY image list JSON = %+v, want %+v", raw.Images, imageCatalog)
	}

	assertNoANSI(t, runTerm(t, client, "image list", "dumb", 0).out)
}

func TestSSHImageSelectionAndOwnership(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)
	other := s.connect(t, bob)

	created := run(t, client, "new --name=demo --image=ubuntu-24.04 --cpu=2 --memory=512MiB --processes=64 --disk=1GiB --json", nil, 0)
	assertImageInstance(t, created.out, "demo", ubuntuImageFingerprint)
	want := core.CreateOptions{Name: "demo", Image: "ubuntu-24.04", CPU: 2, Memory: "512MiB", Processes: 64, Disk: "1GiB"}
	if got := s.backend.lastCreate(); got != want {
		t.Fatalf("create options = %+v, want %+v", got, want)
	}
	// A guest exec would mean image selection was routed through container
	// execution; adapter argv and launch arguments are covered by unit tests.
	if got := s.backend.lastExec(); got.command != "" {
		t.Fatalf("image selection issued a guest exec: %q", got.command)
	}
	assertImagesListed(t, run(t, client, "ls --json", nil, 0), map[string]string{"demo": ubuntuImageFingerprint})

	byFingerprint := run(t, client, "new --name=pinned --image="+debianImageFingerprint+" --json", nil, 0)
	assertImageInstance(t, byFingerprint.out, "pinned", debianImageFingerprint)
	if got := s.backend.lastCreate(); got.Image != debianImageFingerprint {
		t.Fatalf("fingerprint selection passed %q", got.Image)
	}

	prefixed := run(t, client, "new --name=prefixed --image=local:debian-12 --json", nil, 0)
	assertImageInstance(t, prefixed.out, "prefixed", debianImageFingerprint)
	if got := s.backend.lastCreate(); got.Image != "local:debian-12" {
		t.Fatalf("local: selection passed %q", got.Image)
	}

	// A default create keeps the operator image and must not invent image
	// metadata of its own.
	plain := run(t, client, "new --name=plain --json", nil, 0)
	var plainObject map[string]json.RawMessage
	decode(t, plain.out, &plainObject)
	if plainObject["image"] != nil {
		t.Fatalf("default create invented an image: %s", plain.out)
	}
	if got := s.backend.lastCreate(); got != (core.CreateOptions{Name: "plain"}) {
		t.Fatalf("default create options = %+v", got)
	}

	wantAlice := map[string]string{
		"demo":     ubuntuImageFingerprint,
		"pinned":   debianImageFingerprint,
		"prefixed": debianImageFingerprint,
		"plain":    "",
	}
	assertImagesListed(t, run(t, client, "ls --json", nil, 0), wantAlice)
	assertList(t, run(t, other, "ls --json", nil, 0))

	for _, line := range []string{"rm demo", "restart demo", "ssh demo id"} {
		response := run(t, other, line, nil, 1)
		if response.out != "" || !strings.Contains(response.err, "not owned") {
			t.Fatalf("foreign access %q: %+v", line, response)
		}
	}

	// Selection is per-user: bob shares the operator catalog but creates his
	// own instance from it, leaving alice's image pins untouched.
	bobCreated := run(t, other, "new --name=demo --image=debian-12 --json", nil, 0)
	assertImageInstance(t, bobCreated.out, "demo", debianImageFingerprint)
	assertImagesListed(t, run(t, other, "ls --json", nil, 0), map[string]string{"demo": debianImageFingerprint})
	assertImagesListed(t, run(t, client, "ls --json", nil, 0), wantAlice)
}

func TestSSHImageFailuresStayLocal(t *testing.T) {
	clearNoColor(t)
	s := startGateway(t)
	client := s.connect(t, alice)

	// Remote, OCI and host-style references, plus every cache mutation, are
	// rejected before the backend is reached.
	before := s.backend.callCount()
	for _, line := range []string{
		"new --name=demo --image=images:ubuntu/24.04 --json",
		"new --name=demo --image=docker:alpine --json",
		"new --name=demo --image=oci:ubuntu --json",
		"new --name=demo --image=https://example.invalid/ubuntu --json",
		"new --name=demo --image=../ubuntu --json",
		"image list images:ubuntu --json",
		"image list --remote=images --json",
		"image add ubuntu-24.04 --json",
		"image remove ubuntu-24.04 --json",
		"image rm ubuntu-24.04 --json",
		"image delete ubuntu-24.04 --json",
		"image import ubuntu-24.04 --json",
		"image export ubuntu-24.04 --json",
		"image copy ubuntu-24.04 --json",
		"image pull ubuntu-24.04 --json",
		"image push ubuntu-24.04 --json",
		"image refresh ubuntu-24.04 --json",
		"image alias ubuntu-24.04 --json",
		"image set ubuntu-24.04 --json",
		"image unset ubuntu-24.04 --json",
		"image edit ubuntu-24.04 --json",
		"image --json",
	} {
		response := run(t, client, line, nil, 1)
		var rejected struct {
			Error string `json:"error"`
		}
		assertRawJSON(t, response.out, &rejected)
		if rejected.Error == "" || response.err != "" {
			t.Fatalf("%q = %+v; want a JSON rejection on stdout only", line, response)
		}
	}
	if got := s.backend.callCount(); got != before {
		t.Fatalf("rejected image choices reached the backend: calls %d -> %d", before, got)
	}
	if got := s.backend.lastExec(); got.command != "" {
		t.Fatalf("rejected image choices issued a guest exec: %q", got.command)
	}

	// An alias missing from the fake cache is refused before creation. The real
	// adapter's launch argv and cache-mutation refusal are covered by its unit
	// tests; this wire test only proves the fake never creates the instance.
	missing := run(t, client, "new --name=missing --image=centos-9 --json", nil, 1)
	var rejected struct {
		Error string `json:"error"`
	}
	assertRawJSON(t, missing.out, &rejected)
	if !strings.Contains(rejected.Error, "not cached") || missing.err != "" {
		t.Fatalf("uncached image = %+v; want a JSON not-cached error", missing)
	}
	if got := s.backend.lastCreate().Image; got != "centos-9" {
		t.Fatalf("backend saw image %q, want the rejected choice for validation", got)
	}
	if got := s.backend.lastExec(); got.command != "" {
		t.Fatalf("uncached image issued a guest exec: %q", got.command)
	}
	assertList(t, run(t, client, "ls --json", nil, 0))
}

func assertImageCatalog(t *testing.T, response response) {
	t.Helper()
	var object map[string]json.RawMessage
	decode(t, response.out, &object)
	if len(object) != 1 || object["images"] == nil {
		t.Fatalf("image list JSON shape = %s, want only an images key", response.out)
	}
	var got []core.ImageInfo
	decode(t, string(object["images"]), &got)
	if response.err != "" || !reflect.DeepEqual(got, imageCatalog) {
		t.Fatalf("image list JSON = %+v stderr %q, want %+v", got, response.err, imageCatalog)
	}
}

func assertImageInstance(t *testing.T, text, name, fingerprint string) {
	t.Helper()
	var item map[string]json.RawMessage
	decode(t, text, &item)
	if item["image"] == nil {
		t.Fatalf("instance JSON omits the image fingerprint: %s", text)
	}
	var gotName, gotImage string
	decode(t, string(item["vm_name"]), &gotName)
	decode(t, string(item["image"]), &gotImage)
	if gotName != name || gotImage != fingerprint || item["status"] == nil {
		t.Fatalf("instance JSON = %s; want name %q image %q", text, name, fingerprint)
	}
}

func assertImagesListed(t *testing.T, response response, want map[string]string) {
	t.Helper()
	var listed struct {
		VMs []struct {
			Name  string `json:"vm_name"`
			Image string `json:"image"`
		} `json:"vms"`
	}
	decode(t, response.out, &listed)
	if response.err != "" || len(listed.VMs) != len(want) {
		t.Fatalf("ls = %+v, want %d instances", response, len(want))
	}
	for _, vm := range listed.VMs {
		wantImage, known := want[vm.Name]
		if !known || vm.Image != wantImage {
			t.Fatalf("instance %q image = %q, want %q", vm.Name, vm.Image, wantImage)
		}
	}
}
