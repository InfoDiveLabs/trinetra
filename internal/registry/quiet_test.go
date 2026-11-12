package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestRefreshWithoutKeysDoesNotLog(t *testing.T) {
	var logs []string
	r := New(Options{Dir: t.TempDir(), Logf: func(f string, a ...any) { logs = append(logs, f) }})
	if err := r.Refresh(context.Background()); !errors.Is(err, plugin.ErrNoRegistryKeys) {
		t.Fatalf("err = %v, want ErrNoRegistryKeys", err)
	}
	if len(logs) != 0 {
		t.Fatalf("logged %d lines for a build without registry keys: %v", len(logs), logs)
	}
}
