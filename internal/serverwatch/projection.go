package serverwatch

import "serverwatch/internal/config"

// projectMountDaysToFull queries store's "disk:<mount>" series over roughly
// the last 7 days (at whichever resolution PickResolution says covers that
// window relative to nowUnix) and runs projectDaysToFull over the result,
// using currentPct as the reference point. ok is false whenever store is
// nil (no SampleStore configured, or a one-shot caller with none handy), the
// query itself errors, or the projection isn't meaningful (see
// projectDaysToFull). c may be nil (falls back to defaultRawRetention),
// mirroring collectSlow's other nil-config guards.
func projectMountDaysToFull(store SampleStore, mount string, currentPct float64, c *config.Config, nowUnix int64) (days float64, ok bool) {
	if store == nil {
		return 0, false
	}
	rawRet := defaultRawRetention
	if c != nil {
		rawRet = configuredRawRetention(c)
	}
	const sevenDaysSecs = 7 * 24 * 3600
	since := nowUnix - sevenDaysSecs
	res := PickResolution(since, nowUnix, nowUnix, rawRet)
	pts, err := store.Query("disk:"+mount, since, nowUnix, res)
	if err != nil {
		return 0, false
	}
	return projectDaysToFull(pts, currentPct)
}

// projectDaysToFull fits a linear least-squares trend line to pts (x = TS in
// unix seconds, y = Max — the peak usage percentage within each point's
// bucket) and projects how many days until that trend would cross 100%,
// starting from currentPct (the freshest known usage percentage, which may
// differ slightly from pts' own last value if it was collected more
// recently than the store's last append).
//
// ok is false — and days is meaningless — whenever the projection wouldn't
// be meaningful:
//   - fewer than 2 points (can't fit a line through one point);
//   - a non-positive slope (flat or declining usage never reaches 100%).
//
// This is a pure function over Points; callers (collectSlow) are
// responsible for querying the right window (PickResolution over the
// mount's "disk:<mount>" series) and guarding a nil store before calling it.
func projectDaysToFull(pts []Point, currentPct float64) (days float64, ok bool) {
	n := len(pts)
	if n < 2 {
		return 0, false
	}
	var sumX, sumY, sumXY, sumXX float64
	for _, p := range pts {
		x := float64(p.TS)
		y := p.Max
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	nf := float64(n)
	denom := nf*sumXX - sumX*sumX
	if denom == 0 {
		return 0, false
	}
	slopePerSec := (nf*sumXY - sumX*sumY) / denom
	if slopePerSec <= 0 {
		return 0, false
	}
	const secondsPerDay = 86400
	slopePerDay := slopePerSec * secondsPerDay
	if slopePerDay <= 0 {
		return 0, false
	}
	days = (100 - currentPct) / slopePerDay
	if days < 0 {
		days = 0
	}
	return days, true
}
