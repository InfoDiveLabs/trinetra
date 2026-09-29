//go:build !trinetra_testkeys

// internal/update/keys.go
package update

// productionKeyB64 holds the base64 ed25519 public keys trusted by release
// builds, current key first, next key second, per role. They are filled in
// by the maintainer key ceremony (see docs/handbook/10-operations.md,
// "Release keys"). While a role is empty every update and signed install
// fails closed with ErrNoKeys.
var productionKeyB64 = struct{ CI, Maint, Pointer []string }{
	CI:      []string{},
	Maint:   []string{},
	Pointer: []string{},
}

func ProductionKeys() KeySet {
	return mustKeySet(productionKeyB64.CI, productionKeyB64.Maint, productionKeyB64.Pointer)
}
