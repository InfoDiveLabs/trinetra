package registry

// TestSeed is the deterministic seed of the trinetra_testkeys build's registry key (test-only).
var TestSeed = func() (s [32]byte) {
	copy(s[:], "trinetra-registry-test-key-0001")
	return
}()
