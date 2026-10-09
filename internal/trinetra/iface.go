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

// execTimeout bounds each external command.
var execTimeout = 60 * time.Second

func (osExec) Run(name string, args ...string) ([]byte, error) {
	// Bound external commands: a hung df/docker/systemctl/smartctl would
	// otherwise block the caller forever.
	return runWithTimeout(execTimeout, name, args...)
}

// runWithTimeout runs name with args under a hard deadline, killing the whole process group
// on expiry -- otherwise a grandchild.
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
