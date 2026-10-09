package trinetra

import "sync/atomic"

// slowHub carries the latest successful slow-tier Snapshot from the slow-collector
// goroutine to the sampler loop.
type slowHub struct {
	snap    atomic.Pointer[Snapshot]
	version atomic.Uint64
}

func (h *slowHub) publish(s Snapshot) {
	h.snap.Store(&s)
	h.version.Add(1)
}

func (h *slowHub) latest() (Snapshot, uint64, bool) {
	p := h.snap.Load()
	if p == nil {
		return Snapshot{}, 0, false
	}
	return *p, h.version.Load(), true
}
