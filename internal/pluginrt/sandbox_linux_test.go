//go:build linux

package pluginrt

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

const rlimitChildEnv = "TRINETRA_RLIMIT_CHILD"

func TestRlimitsLetAGoProgramStart(t *testing.T) {
	if os.Getenv(rlimitChildEnv) == "apply" {
		rl, err := Rlimits(DefaultLimits)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rl {
			if r.Resource == rlimitNproc {
				continue
			}
			if err := syscall.Setrlimit(r.Resource, &syscall.Rlimit{Cur: r.Cur, Max: r.Max}); err != nil {
				t.Fatal(err)
			}
		}
		env := append(os.Environ(), rlimitChildEnv+"=run")
		if err := syscall.Exec(os.Args[0], []string{os.Args[0], "-test.run=^$"}, env); err != nil {
			t.Fatal(err)
		}
		return
	}
	if os.Getenv(rlimitChildEnv) != "" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRlimitsLetAGoProgramStart$")
	cmd.Env = append(os.Environ(), rlimitChildEnv+"=apply")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("a Go program could not start under the plugin rlimits: %v\n%s", err, out)
	}
}
