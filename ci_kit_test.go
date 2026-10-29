package main

// go.mod, Block CI's uses: line and its kit-ref name one block-kit release, at least v0.5.5, the first whose shared workflow runs the secrets and scripts checks main requires.

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

var kitPins = []struct{ file, pattern string }{
	{"go.mod", `(?m)^\s*(?:require )?github\.com/mitstack-eu/block-kit (v\d+\.\d+\.\d+)$`},
	{".github/workflows/ci.yml", `(?m)^\s+uses: mitstack-eu/block-kit/\.github/workflows/publish-block\.yml@(v\d+\.\d+\.\d+)$`},
	{".github/workflows/ci.yml", `(?m)^\s+kit-ref: (v\d+\.\d+\.\d+)$`},
}

func TestBlockCI_RunsTheBlockKitReleaseGoModRequiresAndThatReleaseRunsTheSharedScans(t *testing.T) {
	versions := map[string]bool{}
	for _, pin := range kitPins {
		raw, err := os.ReadFile(pin.file)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(pin.pattern).FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s has no line matching %s", pin.file, pin.pattern)
		}
		versions[string(m[1])] = true
	}

	if len(versions) != 1 {
		t.Fatalf("go.mod, uses: and kit-ref name %v; want one block-kit release", versions)
	}
	for v := range versions {
		if !atLeast(v, 0, 5, 5) {
			t.Errorf("Block CI runs block-kit %s; want v0.5.5 or later, whose publish-block runs the block / secrets and block / scripts checks main requires", v)
		}
	}
}

func atLeast(v string, want ...int) bool {
	parts := regexp.MustCompile(`\d+`).FindAllString(v, 3)
	for i, w := range want {
		n, _ := strconv.Atoi(parts[i])
		if n != w {
			return n > w
		}
	}
	return true
}
