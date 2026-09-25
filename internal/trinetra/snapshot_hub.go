package trinetra

import "sync/atomic"

// snapshotHub holds the latest merged Snapshot the sampler loop has built,
// updated once per fast tick (see cmdDaemon in daemon.go). Reads (the
// untagged in-process core.API, newInprocAPI's getSnap, and the control
// socket it backs -- coreapi_inproc.go/control_socket.go) are lock-free via
// atomic.Pointer; the sampler loop is the sole writer.
var snapshotHub atomic.Pointer[Snapshot]

// latestSnapshot returns the most recent Snapshot stored in snapshotHub, or
// the zero Snapshot if the sampler loop hasn't published one yet (e.g. a
// control-socket client calling this before cmdDaemon's loop has run its
// first tick).
func latestSnapshot() Snapshot {
	if s := snapshotHub.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}
