package trinetra

import (
	"reflect"
	"testing"
)

// TestLatestSnapshotDefaultsToZeroValue pins snapshotHub's read side before
// the sampler loop has ever published one (e.g. a control-socket client
// calling Snapshot() before the first fast tick).
func TestLatestSnapshotDefaultsToZeroValue(t *testing.T) {
	old := snapshotHub.Load()
	t.Cleanup(func() { snapshotHub.Store(old) })
	snapshotHub.Store(nil)
	if got := latestSnapshot(); !reflect.DeepEqual(got, Snapshot{}) {
		t.Fatalf("latestSnapshot() = %+v, want zero value", got)
	}
}
