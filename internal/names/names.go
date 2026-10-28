// Package names generates exe.dev-style fruit names for new containers.
package names

import (
	"crypto/rand"
	"errors"
	"io"
	"math/big"
	"strconv"
	"strings"
)

var fruits = []string{
	"apple", "apricot", "banana", "blackberry", "blueberry", "cherry",
	"coconut", "cranberry", "fig", "grape", "guava", "kiwi", "lemon",
	"lime", "lychee", "mango", "melon", "nectarine", "orange", "papaya",
	"peach", "pear", "pineapple", "plum", "pomegranate", "quince",
	"raspberry", "strawberry", "tangerine", "watermelon",
}

// Pick chooses uniformly among free bare fruit names. Bare names win over
// suffixed ones so a fresh container is named "mango", not "mango-57". Only
// when every bare name is taken does it fall back to suffixes: each fruit
// contributes its lowest free suffix in the range 2-100, and the pick is
// uniform across those fruits (so a fruit with a free low suffix is not
// favoured over one that had to climb higher).
func Pick(taken map[string]bool) (string, error) {
	return pick(taken, rand.Reader)
}

func pick(taken map[string]bool, r io.Reader) (string, error) {
	bare := make([]string, 0, len(fruits))
	for _, fruit := range fruits {
		if !taken[fruit] {
			bare = append(bare, fruit)
		}
	}
	if len(bare) > 0 {
		return choose(r, bare)
	}

	suffixed := make([]string, 0, len(fruits))
	for _, fruit := range fruits {
		for i := 2; i <= 100; i++ {
			candidate := fruit + "-" + strconv.Itoa(i)
			if !taken[candidate] {
				suffixed = append(suffixed, candidate)
				break
			}
		}
	}
	if len(suffixed) == 0 {
		return "", errors.New("no instance name available")
	}
	return choose(r, suffixed)
}

func choose(r io.Reader, candidates []string) (string, error) {
	n, err := rand.Int(r, big.NewInt(int64(len(candidates))))
	if err != nil {
		return "", errors.New("cannot generate instance name")
	}
	return candidates[n.Int64()], nil
}

// Valid reports whether name is a fruit or a fruit with a suffix in the range 2-100.
func Valid(name string) bool {
	base, suffix, ok := strings.Cut(name, "-")
	if ok {
		if suffix == "" || (len(suffix) > 1 && suffix[0] == '0') {
			return false
		}
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 2 || n > 100 || strconv.Itoa(n) != suffix {
			return false
		}
	}
	for _, fruit := range fruits {
		if fruit == base {
			return true
		}
	}
	return false
}
