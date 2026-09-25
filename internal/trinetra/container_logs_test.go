package trinetra

import (
	"strings"
	"testing"
)

func TestValidContainerName(t *testing.T) {
	ok := []string{"web", "my-app_1", "db.1", "a", "0abc", "Container-Name.v2"}
	bad := []string{"", "-web", ".hidden", "_x", "web; rm -rf /", "--since", "a b", "name\n", "foo|bar", strings.Repeat("x", 129)}
	for _, n := range ok {
		if !validContainerName(n) {
			t.Errorf("validContainerName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if validContainerName(n) {
			t.Errorf("validContainerName(%q) = true, want false", n)
		}
	}
}

func TestCollectContainerLogs(t *testing.T) {
	fs := fakeFS{files: map[string]string{"/var/run/docker.sock": ""}}
	// Exec that answers `docker ps` with two containers and `docker logs` with
	// fixed output, and records the exact argv it was asked to run.
	var lastArgs []string
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		lastArgs = append([]string{name}, args...)
		if len(args) > 0 && args[0] == "ps" {
			return []byte("web\trunning\tUp 2 hours\ndb\trunning\tUp 2 hours\n"), nil
		}
		if len(args) > 0 && args[0] == "logs" {
			return []byte("line one\nline two\n"), nil
		}
		return nil, nil
	}}

	// Happy path: a known container returns its logs, and the argv is exactly
	// docker logs --tail N <name> (no shell, no extra flags).
	out, err := collectContainerLogs(x, fs, "web", 50)
	if err != nil {
		t.Fatalf("collectContainerLogs(web) error: %v", err)
	}
	if !strings.Contains(out, "line one") || !strings.Contains(out, "line two") {
		t.Errorf("logs = %q, want the two lines", out)
	}
	want := []string{"docker", "logs", "--tail", "50", "web"}
	if strings.Join(lastArgs, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", lastArgs, want)
	}

	// Unknown container: refused before shelling out to `docker logs`.
	if _, err := collectContainerLogs(x, fs, "nope", 50); err == nil {
		t.Error("collectContainerLogs(unknown) = nil error, want refusal")
	}

	// Malformed name: refused by validation, never reaches docker ps.
	if _, err := collectContainerLogs(x, fs, "--since", 50); err == nil {
		t.Error("collectContainerLogs(flag-like name) = nil error, want refusal")
	}
}
