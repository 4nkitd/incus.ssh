package names

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

// indexReader feeds a repeated byte so crypto/rand.Int resolves to that
// index (candidates are shorter than 32), independent of OS entropy and of
// any stray probe reads crypto/rand may perform.
type indexReader struct{ b []byte }

func (r *indexReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	p[0] = r.b[0]
	r.b = r.b[1:]
	return 1, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func index(n int) io.Reader {
	return &indexReader{b: bytes.Repeat([]byte{byte(n)}, 16)}
}

func fixedPick(t *testing.T, taken map[string]bool, n int) string {
	t.Helper()
	got, err := pick(taken, index(n))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPickBareNamesFirst(t *testing.T) {
	if got := fixedPick(t, map[string]bool{}, 0); got != "apple" {
		t.Fatalf("empty project picked %q, want apple", got)
	}
	taken := map[string]bool{"apple": true, "mango": true}
	got := fixedPick(t, taken, 0)
	if got != "apricot" {
		t.Fatalf("picked %q, want apricot", got)
	}
	if strings.Contains(got, "-") {
		t.Fatalf("bare names available but got suffix %q", got)
	}
}

func TestPickCoversEveryFreeBareName(t *testing.T) {
	taken := map[string]bool{"apple": true, "mango": true, "kiwi": true}
	want := make([]string, 0, len(fruits))
	for _, fruit := range fruits {
		if !taken[fruit] {
			want = append(want, fruit)
		}
	}
	seen := make(map[string]bool, len(want))
	for i := range want {
		got := fixedPick(t, taken, i)
		if got != want[i] {
			t.Fatalf("index %d picked %q, want %q", i, got, want[i])
		}
		if seen[got] {
			t.Fatalf("duplicate pick %q", got)
		}
		seen[got] = true
		if !Valid(got) {
			t.Fatalf("invalid pick %q", got)
		}
	}
}

func TestPickSuffixOnlyWhenBareExhausted(t *testing.T) {
	taken := map[string]bool{}
	for _, fruit := range fruits {
		taken[fruit] = true
	}
	if got := fixedPick(t, taken, 0); got != "apple-2" {
		t.Fatalf("got %q, want apple-2", got)
	}
	taken["apple-2"] = true
	if got := fixedPick(t, taken, 0); got != "apple-3" {
		t.Fatalf("got %q, want apple-3", got)
	}
	taken["apple-3"] = true
	if got := fixedPick(t, taken, 0); got != "apple-4" {
		t.Fatalf("got %q, want apple-4", got)
	}
}

func TestPickUsesLowestFreeSuffixPerFruit(t *testing.T) {
	taken := map[string]bool{}
	for _, fruit := range fruits {
		taken[fruit] = true
	}
	for i := 2; i <= 9; i++ {
		taken["apple-"+strconv.Itoa(i)] = true
	}
	if got := fixedPick(t, taken, 0); got != "apple-10" {
		t.Fatalf("got %q, want apple-10", got)
	}
}

func TestPickExhaustedFruitDoesNotFailOthers(t *testing.T) {
	taken := map[string]bool{}
	for _, fruit := range fruits {
		taken[fruit] = true
	}
	for i := 2; i <= 100; i++ {
		taken["mango-"+strconv.Itoa(i)] = true
	}
	seen := make(map[string]bool)
	for i := 0; i < len(fruits)-1; i++ {
		got := fixedPick(t, taken, i)
		if strings.HasPrefix(got, "mango") {
			t.Fatalf("picked exhausted fruit %q", got)
		}
		if !Valid(got) {
			t.Fatalf("invalid pick %q", got)
		}
		seen[got] = true
	}
	if len(seen) != len(fruits)-1 {
		t.Fatalf("got %d distinct picks, want %d", len(seen), len(fruits)-1)
	}
}

func TestPickGlobalExhaustion(t *testing.T) {
	taken := map[string]bool{}
	for _, fruit := range fruits {
		taken[fruit] = true
		for i := 2; i <= 100; i++ {
			taken[fruit+"-"+strconv.Itoa(i)] = true
		}
	}
	if _, err := pick(taken, index(0)); err == nil {
		t.Fatal("expected exhaustion error")
	}
}

func TestPickPropagatesReaderError(t *testing.T) {
	if _, err := pick(map[string]bool{}, errReader{}); err == nil {
		t.Fatal("expected entropy error")
	}
}

func TestValid(t *testing.T) {
	for _, name := range []string{"mango", "mango-2", "mango-100", "watermelon"} {
		if !Valid(name) {
			t.Errorf("rejected %q", name)
		}
	}
	for _, name := range []string{"", "mango-1", "mango-101", "mango-+2", "mango-02", "mango-0002", "dev-abc", "mango-x", "mango-", "dragonfruit", "dragonfruit-2", "mango- 2"} {
		if Valid(name) {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestNamesAreSlugs(t *testing.T) {
	for _, fruit := range fruits {
		if fruit == "" || strings.ToLower(fruit) != fruit || strings.HasPrefix(fruit, "-") {
			t.Fatalf("invalid fruit name %q", fruit)
		}
		for _, r := range fruit {
			if !(r >= 'a' && r <= 'z') {
				t.Fatalf("invalid character in %q", fruit)
			}
		}
	}
}
