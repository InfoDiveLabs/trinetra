// Package core defines the data types and the core.API interface that the
// web UI, the CLI, and the control-socket client all consume as their one
// boundary onto daemon state.
//
// Import contract (enforced by internal/trinetra/buildtag_test.go's
// TestDefaultBuildIsStdlibOnly, which scans the untagged build's dependency
// graph): this package may import ONLY the Go standard library and
// trinetra/internal/config. It must never import internal/web or
// internal/trinetra, in either build direction, so that internal/web
// (which may import core) and internal/trinetra (which may also import
// core) both stay free of a cycle back into this package.
package core
