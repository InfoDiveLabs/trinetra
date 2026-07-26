package serverwatch

import (
	"reflect"
	"testing"
)

// TestMaybeStartWebNoopInDefaultBuild pins the default (!web) build's half
// of the seam: maybeStartWeb must accept a WebDeps and return a stop func
// that is safe to call, without starting anything (there is no web server in
// this build). This is the contract cmdDaemon relies on regardless of which
// build variant is linked in.
func TestMaybeStartWebNoopInDefaultBuild(t *testing.T) {
	stop := maybeStartWeb(WebDeps{})
	if stop == nil {
		t.Fatal("maybeStartWeb returned a nil stop func")
	}
	stop() // must not panic
}

// TestLatestSnapshotDefaultsToZeroValue pins snapshotHub's read side before
// the sampler loop has ever published one (e.g. a hypothetical future web
// server calling Snapshot() before the first fast tick).
func TestLatestSnapshotDefaultsToZeroValue(t *testing.T) {
	old := snapshotHub.Load()
	t.Cleanup(func() { snapshotHub.Store(old) })
	snapshotHub.Store(nil)
	if got := latestSnapshot(); !reflect.DeepEqual(got, Snapshot{}) {
		t.Fatalf("latestSnapshot() = %+v, want zero value", got)
	}
}
