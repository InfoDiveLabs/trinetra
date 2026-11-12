//go:build !trinetra_testkeys

package registry

import (
	"bytes"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func TestProductionRegistryKeysDecode(t *testing.T) {
	for _, k := range Keys() {
		if len(k) != 32 {
			t.Fatalf("bad key length %d", len(k))
		}
	}
}

func TestRegistryKeysDistinctFromReleaseKeys(t *testing.T) {
	rel := update.ProductionKeys()
	all := append(append(append([]update.PublicKey{}, rel.CI...), rel.Maint...), rel.Pointer...)
	for _, k := range Keys() {
		for _, r := range all {
			if bytes.Equal(k, r) {
				t.Fatal("registry key overlaps a release key")
			}
		}
	}
}
