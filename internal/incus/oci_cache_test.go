package incus

import (
	"context"
	"strings"
	"testing"

	"incus-for-devs/internal/core"
)

func TestOCICacheDoesNotBecomeNativeImageCatalog(t *testing.T) {
	oci := fakeImage{Fingerprint: strings.Repeat("a", 64), Type: "container", Aliases: []fakeImageAlias{{Name: "web"}}, Properties: map[string]string{"type": "oci"}}
	native := fakeImage{Fingerprint: strings.Repeat("b", 64), Type: "container", Aliases: []fakeImageAlias{{Name: "ubuntu"}}}
	f := newImageFixture(t, nil, []fakeImage{oci, native}, scenario{})
	images, err := f.backend.Images(context.Background(), alice)
	if err != nil || len(images) != 1 || images[0].Fingerprint != native.Fingerprint {
		t.Fatal("OCI leaked into native catalog", err)
	}
	for _, ref := range []string{"web", oci.Fingerprint} {
		if _, err := f.backend.Create(context.Background(), alice, core.CreateOptions{Name: "escape", Image: ref}); err == nil {
			t.Fatal("native creation bypassed app deployment")
		}
	}
	for _, call := range f.calls(t) {
		if len(call) > 4 && call[4] == "launch" {
			t.Fatal("rejected OCI selection reached launch")
		}
	}
}
