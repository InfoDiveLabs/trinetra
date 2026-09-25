package web

// LiveEvent is internal/web's own mirror of core.Event (internal/core/dto.go)
// -- the Kind/Severity/Source/Title/Time shape core.API.Subscribe's live
// stream carries -- kept as a package-local type rather than importing
// core.Event directly into this package's SSE consumer path (sse.go, Deps'
// Subscribe field). The trinetra-web binary's buildDeps
// (cmd/trinetra-web/main.go) is the one place that adapts a real
// core.Event into a LiveEvent (a trivial field copy); internal/web never
// needs to know core.Event exists to consume the stream.
type LiveEvent struct {
	// Kind identifies what happened: "snapshot" for the lightweight
	// snapshot-changed tick that tells eventsHandler/publicEventsHandler to
	// re-fetch Deps.Snapshot(), or an alert-shaped kind (for example
	// "alert_fire"/"alert_recover") for everything else.
	Kind string `json:"kind"`
	// Severity/Source/Title mirror an alert-shaped event's identically named
	// fields; empty/unused for Kind == "snapshot".
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	// Time is the Unix-seconds timestamp the event occurred.
	Time int64 `json:"time"`
}
