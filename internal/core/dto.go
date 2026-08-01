package core

// Resolution selects the granularity a Series query is served at: raw
// per-sample points, or the coarser 1-minute rollups.
type Resolution int

const (
	ResRaw Resolution = iota
	Res1m
)

// SeriesPoint is one time-series sample: TS is Unix seconds, Min/Avg/Max
// are the same value for a raw point, or a real rollup for a downsampled
// one.
type SeriesPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// DownEventView is one downtime event (a target or interface going down
// and, if it has ended, coming back), as rendered to a consumer: Type
// identifies what went down (for example "net_down"), Start/End are Unix
// seconds (End is 0 while the event is still open), and DurationSec is the
// event's length in seconds.
type DownEventView struct {
	Type        string `json:"type"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DurationSec int64  `json:"duration_sec"`
}
