// Package core defines the data types and (in later tasks) the core.API
// interface that both the embedded web UI and the CLI consume as their one
// boundary onto daemon state.
//
// Import contract (enforced by internal/serverwatch/buildtag_test.go's
// TestDefaultBuildIsStdlibOnly, which scans the untagged build's dependency
// graph): this package may import ONLY the Go standard library and
// serverwatch/internal/config. It must never import internal/web or
// internal/serverwatch, in either build direction, so that internal/web
// (which may import core) and internal/serverwatch (which may also import
// core) both stay free of a cycle back into this package.
package core
