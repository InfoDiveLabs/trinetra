package trinetra

import (
	"sync"
	"time"
)

// shutdownHooks are run by the SIGTERM handler, which exits without unwinding deferred calls.
type shutdownHooks struct {
	mu sync.Mutex
	fs []func()
}

func (h *shutdownHooks) add(f func()) {
	h.mu.Lock()
	h.fs = append(h.fs, f)
	h.mu.Unlock()
}

// run calls the hooks newest first and returns when they finish or timeout passes.
func (h *shutdownHooks) run(timeout time.Duration) {
	h.mu.Lock()
	fs := append([]func(){}, h.fs...)
	h.mu.Unlock()
	done := make(chan struct{})
	go func() {
		for i := len(fs) - 1; i >= 0; i-- {
			fs[i]()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
