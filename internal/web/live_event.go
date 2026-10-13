package web

// LiveEvent is internal/web's own mirror of core.Event (internal/core/dto.go) -- the
// Kind/Severity/Source/Title/Time shape core.API.Subscribe's live stream carries.
type LiveEvent struct {
	// Kind identifies what happened: "snapshot" for the lightweight snapshot-changed tick that
	// tells eventsHandler/publicEventsHandler to re-fetch Deps.Snapshot().
	Kind string `json:"kind"`
	// Severity/Source/Title mirror an alert-shaped event's identically named
	// fields; empty/unused for Kind == "snapshot".
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	// Time is the Unix-seconds timestamp the event occurred.
	Time int64 `json:"time"`
}
