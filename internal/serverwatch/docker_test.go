package serverwatch

import (
	"errors"
	"testing"
)

func TestParseDockerPS(t *testing.T) {
	s := "web\trunning\tUp 3 hours\n" +
		"db\texited\tExited (0) 2 hours ago\n"
	cs := parseDockerPS(s)
	if len(cs) != 2 || cs[0].Name != "web" || cs[0].State != "running" {
		t.Fatalf("containers = %+v", cs)
	}
	if cs[1].State != "exited" {
		t.Fatalf("db state = %q", cs[1].State)
	}
}

// fakeExec returns canned output per command name.
type fakeExec struct {
	fn func(name string, args ...string) ([]byte, error)
}

func (f fakeExec) Run(name string, args ...string) ([]byte, error) { return f.fn(name, args...) }

func TestProbeDockerFallsBackToSudo(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" {
			return nil, errors.New("permission denied on /var/run/docker.sock")
		}
		if name == "sudo" && len(args) > 0 && args[0] == "docker" {
			return []byte("web\trunning\tUp\n"), nil
		}
		return nil, errors.New("unexpected")
	}}
	// socket not present in fake fs
	fs := fakeFS{files: map[string]string{}}
	a := probeDocker(x, fs)
	if !a.available || !a.sudo {
		t.Fatalf("expected sudo fallback, got %+v", a)
	}
}
