package trinetra

import (
	"os"
	"sort"
	"strings"
	"testing"
)

func goModRequires(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	inBlock := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "require (":
			inBlock = true
		case line == ")":
			inBlock = false
		case inBlock && line != "":
			out = append(out, line)
		case strings.HasPrefix(line, "require ") && !strings.HasSuffix(line, "("):
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "require ")))
		}
	}
	sort.Strings(out)
	return out
}

func TestGoModRequiresGolden(t *testing.T) {
	want, err := os.ReadFile("testdata/gomod_requires.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got := strings.Join(goModRequires(t), "\n") + "\n"
	if got != string(want) {
		t.Fatalf("go.mod requires changed: the plugin system must not add third-party modules (spec §4)\n--- want\n%s--- got\n%s", want, got)
	}
}
