// Package fleet implements trinetra's master/child fleet link: the private
// PKI and join tokens a master uses to enroll children, the durable outbox a
// child spools telemetry into, the wire format and HTTP endpoints that carry
// it over mutual TLS, and the liveness model the master uses to decide a node
// is down.
//
// This package is imported by the core daemon (cmd/trinetra) and so must
// stay standard-library only -- see internal/trinetra/buildtag_test.go.
// It must not import internal/trinetra; the daemon adapts to it through
// the small Sink and GapFiller interfaces instead.
package fleet
