package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/registry/regtool"
)

const scaffoldDir = "../../registry"

func TestRegistryScaffoldFilesExist(t *testing.T) {
	for _, f := range []string{
		"README.md", "CONTRIBUTING.md", "revoked.json",
		"plugins/README.md",
		"plugins/_template/plugin.json", "plugins/_template/main.go", "plugins/_template/main_test.go", "plugins/_template/README.md",
		".github/workflows/pr.yml", ".github/workflows/publish.yml", ".github/workflows/resign.yml",
		"scripts/build-all.sh",
	} {
		if st, err := os.Stat(filepath.Join(scaffoldDir, f)); err != nil || st.IsDir() {
			t.Errorf("registry/%s missing", f)
		}
	}
	b, _ := os.ReadFile(filepath.Join(scaffoldDir, "revoked.json"))
	if string(b) != "[]\n" {
		t.Errorf("revoked.json = %q", b)
	}
}

func TestTemplatePassesCheck(t *testing.T) {
	if err := regtool.Check(filepath.Join(scaffoldDir, "plugins/_template")); err != nil {
		t.Fatal(err)
	}
}

var subcommandRef = regexp.MustCompile(`(?:^|[\s(])trinetra-registry\s+([a-z][a-z-]*)`)

func TestWorkflowsReferenceExistingSubcommands(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(scaffoldDir, ".github/workflows/*.yml"))
	scripts, _ := filepath.Glob(filepath.Join(scaffoldDir, "scripts/*.sh"))
	files = append(files, scripts...)
	if len(files) < 4 {
		t.Fatalf("found %d files to scan", len(files))
	}
	used := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range subcommandRef.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
			if _, ok := commands[m[1]]; !ok {
				t.Errorf("%s uses unknown subcommand %q", f, m[1])
			}
		}
	}
	for _, want := range []string{"check", "hash", "sign", "package", "index"} {
		if !used[want] {
			t.Errorf("no workflow or script uses %q", want)
		}
	}
}

func TestBuildScriptSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	if out, err := exec.Command(bash, "-n", filepath.Join(scaffoldDir, "scripts/build-all.sh")).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}
