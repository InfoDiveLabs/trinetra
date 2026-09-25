// Package control implements the daemon's control socket: a newline-
// delimited JSON wire protocol that lets a separate process (a CLI or a
// split-out web process) call core.API over a local unix socket and get the
// same data as an in-process caller.
//
// Import contract (enforced by internal/trinetra/buildtag_test.go's
// TestDefaultBuildIsStdlibOnly, which scans the untagged build's dependency
// graph): this package may import ONLY the Go standard library,
// trinetra/internal/core, and trinetra/internal/config. It must never
// import internal/web or internal/trinetra, in either build direction,
// so the control socket stays usable by any consumer without pulling in
// daemon or web-only dependencies.
package control
