package serverwatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Exec interface {
	Run(name string, args ...string) ([]byte, error)
}

type osExec struct{}

func (osExec) Run(name string, args ...string) ([]byte, error) {
	// Bound external commands: a hung df/docker/systemctl/smartctl would
	// otherwise block the sampler goroutine forever.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type FileSource interface {
	Read(path string) ([]byte, error)
	Glob(pattern string) ([]string, error)
}

type osFS struct{}

func (osFS) Read(path string) ([]byte, error)      { return os.ReadFile(path) }
func (osFS) Glob(pattern string) ([]string, error) { return filepath.Glob(pattern) }
