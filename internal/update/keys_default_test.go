//go:build !trinetra_testkeys

package update

import (
	"errors"
	"reflect"
	"testing"
)

// productionFingerprints pins the release keys from the maintainer key ceremony
// (2026-09-29).
var productionFingerprints = []string{
	"ci:12fa1a1532ab468b3c552db2021b267515bd548bc3e08a0ad4dbc7def3611bb6",
	"ci:2fe78c6f6d7e40ea751cc401952174ffd6da7889a60abcf2949616d9b785d2eb",
	"maint:4becedfe2eea9d5689d6f073869f36b1a082d0ab16e10569660050997696f859",
	"maint:1a77db1b45597594a02371f192c6b4eb115ae437a930b6ee1a11d967b69b4089",
	"pointer:75c9c40cae8c66cee6d266ed276f19b11658a9c1906dcce9dda1fc1cd17750fa",
	"pointer:6eda935c4f3885e91419c01fe751efbb581053f3b8e57a8d36f7cf32a48f0725",
}

func TestProductionKeysArePinned(t *testing.T) {
	if got := Fingerprints(ProductionKeys()); !reflect.DeepEqual(got, productionFingerprints) {
		t.Fatalf("ProductionKeys fingerprints = %v\nwant %v", got, productionFingerprints)
	}
}

// With real keys compiled in, an unsigned or garbage-signed manifest must still be refused.
func TestVerifyReleaseRefusesBadSignatureWithProductionKeys(t *testing.T) {
	_, err := VerifyRelease(ProductionKeys(), []byte("manifest"), []byte("not-a-sig"), []byte("not-a-sig"))
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("VerifyRelease with garbage signatures = %v, want ErrBadSignature", err)
	}
}

// A build with no keys at all must fail closed.
func TestVerifyReleaseFailsClosedWithoutKeys(t *testing.T) {
	if _, err := VerifyRelease(KeySet{}, []byte("manifest"), []byte("sig"), []byte("sig")); err != ErrNoKeys {
		t.Fatalf("VerifyRelease with an empty KeySet = %v, want ErrNoKeys", err)
	}
}
