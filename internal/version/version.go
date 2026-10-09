// Package version carries the product/release version each trinetra binary was built with
// (#107).
package version

import "runtime/debug"

// Version is overwritten at build time by the Makefile's -ldflags -X.
var Version = ""

// String returns the build-stamped version, or a sensible fallback for a plain `go build`
// dev binary: the main module's version from the embedded build info.
func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
