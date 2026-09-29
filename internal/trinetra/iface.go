package trinetra

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Exec interface {
	Run(name string, args ...string) ([]byte, error)
}

type osExec struct{}

// execTimeout bounds each external command. It is a HANG-BREAKER, not a
// performance limit: a monitor must not drop legitimately-slow df/docker/
// systemctl/smartctl output just because a host is big or busy. Since slow
// collection now runs on its own goroutine (it can no longer starve the
// watchdog), a slow command only makes that cycle's data late -- it never
// crashes the daemon -- so this is deliberately generous and operator-tunable
// via the exec_timeout config key. The process-group kill below still recovers
// from a genuinely wedged command. Set once from config at daemon startup
// before any collector goroutine spawns (write-once-before-reads, like
// connDial), so concurrent reads need no lock.
var execTimeout = 60 * time.Second

func (osExec) Run(name string, args ...string) ([]byte, error) {
	// Bound external commands: a hung df/docker/systemctl/smartctl would
	// otherwise block the caller forever.
	return runWithTimeout(execTimeout, name, args...)
}

// runWithTimeout runs name with args under a hard deadline, killing the
// whole process group on expiry -- otherwise a grandchild (e.g. smartctl
// under sudo) keeps the stdout pipe open and CombinedOutput blocks past the
// deadline. Shared by osExec, bound by the package-level execTimeout (a
// generous, operator-tunable ceiling for legitimately slow host commands),
// and timeoutExec (update_apply.go), bound by its own fixed duration for
// callers -- like a self-update smoke test -- that need a much tighter,
// non-configurable bound.
func runWithTimeout(d time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// negative pid => signal the process group led by the child.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second // stop waiting on inherited pipes shortly after Cancel
	return cmd.CombinedOutput()
}

type FileSource interface {
	Read(path string) ([]byte, error)
	Glob(pattern string) ([]string, error)
}

type osFS struct{}

func (osFS) Read(path string) ([]byte, error)      { return os.ReadFile(path) }
func (osFS) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }
