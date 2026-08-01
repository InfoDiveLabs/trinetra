package core

// rawWindowSeconds is the width of a query range below which PickResolution
// still hands back raw points. It matches config.Default()'s
// Storage.RawRetention (48h): the same window the daemon keeps raw samples
// for, so a query no wider than that can be served at full precision, while
// a wider one falls back to 1-minute rollups.
const rawWindowSeconds = 48 * 60 * 60

// PickResolution decides which Resolution a Series query over [from, to]
// (Unix seconds) should be served at: ResRaw for a window no wider than
// rawWindowSeconds, Res1m for anything wider. This is a pure, width-only
// decision aimed at API consumers choosing the Resolution argument to pass
// to a Series call; it is deliberately independent of wall-clock time or
// the store's actual raw-retention configuration, both of which are the
// server-side SampleStore's own concern, not this package's.
func PickResolution(from, to int64) Resolution {
	if to-from <= rawWindowSeconds {
		return ResRaw
	}
	return Res1m
}
