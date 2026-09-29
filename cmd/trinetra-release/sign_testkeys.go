//go:build trinetra_testkeys

// cmd/trinetra-release/sign_testkeys.go
package main

import (
	"os"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// init wires up "sign --role maint-test", which exists only in a
// trinetra_testkeys build: it signs with the deterministic maintainer test
// key (seed 2) so the e2e fixtures can produce a maintainer signature
// without a real maintainer key ceremony.
func init() {
	signMaintTest = func(in, out string) error {
		msg, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		return os.WriteFile(out, update.NewTestSigner(2).SignRelease(msg), 0o644)
	}
}
