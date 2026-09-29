//go:build !trinetra_testkeys

// internal/update/keys.go
package update

// productionKeyB64 holds the base64 ed25519 public keys trusted by release
// builds, current key first, next key second, per role. They are filled in
// by the maintainer key ceremony (see docs/handbook/10-operations.md,
// "Release keys"). While a role is empty every update and signed install
// fails closed with ErrNoKeys.
var productionKeyB64 = struct{ CI, Maint, Pointer []string }{
	CI: []string{
		"qYrzYRct8xy9iJR6Xm8ow+u33GMc8fAoqaX9GZh4+N4=", // current, ci:12fa1a1532ab468b…
		"UMoRKBR5eR908Tu+9wQ6les9m6Aa4pZBbITaJ3QSJuI=", // next, ci:2fe78c6f6d7e40ea…
	},
	Maint: []string{
		"CCoIiySogUWY6OFBHGFfRTGFDOnUp+teJKY/Dxh1uaQ=", // current, maint:4becedfe2eea9d56…
		"/GzPC6QaaouUzZCnnQODcpU07ZStPHIjZ5I1A4fGiAE=", // next, maint:1a77db1b45597594…
	},
	Pointer: []string{
		"kwnelcUBEYplJz/eccyffudFShPlErYGP7S2WGWdCR8=", // current, pointer:75c9c40cae8c66ce…
		"jHQ3krsljylnDlWIZhwPUwxiQzCK5Kg+q+WVE9Icv20=", // next, pointer:6eda935c4f3885e9…
	},
}

func ProductionKeys() KeySet {
	return mustKeySet(productionKeyB64.CI, productionKeyB64.Maint, productionKeyB64.Pointer)
}
