//go:build trinetra_testkeys

package updatetest_test

import (
	"reflect"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
)

// TestProductionKeysAreTheTestKeySetInTestkeysBuild pins the two copies of
// the deterministic key derivation (update's tagged ProductionKeys and this
// package's TestKeySet) to each other.
func TestProductionKeysAreTheTestKeySetInTestkeysBuild(t *testing.T) {
	if got, want := update.ProductionKeys(), updatetest.TestKeySet(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ProductionKeys() = %+v, want TestKeySet() %+v", got, want)
	}
}
