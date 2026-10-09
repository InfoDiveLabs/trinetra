package trinetra

import "github.com/InfoDiveLabs/trinetra/internal/config"

// projectMountDaysToFull queries store's "disk:<mount>" series over roughly the last 7
// days.
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

// projectDaysToFull fits a linear least-squares trend line to pts.
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
