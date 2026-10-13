package trinetra

import "sync/atomic"

// snapshotHub holds the latest merged Snapshot the sampler loop has built, updated once per
// fast tick (see cmdDaemon in daemon.go).
var snapshotHub atomic.Pointer[Snapshot]

// latestSnapshot returns the most recent Snapshot stored in snapshotHub, or the zero
// Snapshot if the sampler loop hasn't published one yet.
func latestSnapshot() Snapshot {
	if s := snapshotHub.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}
