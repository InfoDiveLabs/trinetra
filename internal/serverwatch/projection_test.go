package serverwatch

import (
	"math"
	"testing"
)

func TestProjectDaysToFullRisingSeries(t *testing.T) {
	// One point per day, pct rising by exactly 2%/day: 50, 52, 54, ..., 60
	// over 6 days. currentPct=60 -> want (100-60)/2 = 20 days.
	const day = 86400
	var pts []Point
	for i := 0; i <= 5; i++ {
		pct := 50 + float64(i)*2
		pts = append(pts, Point{TS: int64(i * day), Min: pct, Avg: pct, Max: pct})
	}
	days, ok := projectDaysToFull(pts, 60)
	if !ok {
		t.Fatal("ok = false, want true for a rising series")
	}
	if math.Abs(days-20) > 0.5 {
		t.Fatalf("days = %v, want ~20", days)
	}
}

func TestProjectDaysToFullFlatSeries(t *testing.T) {
	const day = 86400
	var pts []Point
	for i := 0; i <= 5; i++ {
		pts = append(pts, Point{TS: int64(i * day), Min: 40, Avg: 40, Max: 40})
	}
	if _, ok := projectDaysToFull(pts, 40); ok {
		t.Fatal("ok = true, want false for a flat series")
	}
}

func TestProjectDaysToFullDecliningSeries(t *testing.T) {
	const day = 86400
	var pts []Point
	for i := 0; i <= 5; i++ {
		pct := 60 - float64(i)*2
		pts = append(pts, Point{TS: int64(i * day), Min: pct, Avg: pct, Max: pct})
	}
	if _, ok := projectDaysToFull(pts, 50); ok {
		t.Fatal("ok = true, want false for a declining series")
	}
}

func TestProjectDaysToFullTooFewPoints(t *testing.T) {
	if _, ok := projectDaysToFull(nil, 50); ok {
		t.Fatal("ok = true, want false for nil points")
	}
	if _, ok := projectDaysToFull([]Point{{TS: 0, Max: 50}}, 50); ok {
		t.Fatal("ok = true, want false for a single point")
	}
}
