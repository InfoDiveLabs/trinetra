package serverwatch

import (
	"math"
	"testing"
)

// TestAlphaFor checks alphaFor's shape: both values land in (0,1), a
// shorter sampling interval produces a SMALLER alpha, and non-positive
// intervals fall back to the 60s default.
//
// Why smaller, not larger: an EW mean/var's "memory" in units of *samples*
// is ~1/alpha, so its memory in units of *time* is (1/alpha)*intervalSec.
// To keep that time-based window constant (~7 days) across tiers, a metric
// sampled more often (smaller interval) needs a smaller alpha so it takes
// proportionally more samples to span the same wall-clock window. Using one
// global alpha for both tiers is exactly the bug this issue fixes: a fast
// metric (5s) inherited the 60s-tuned alpha and ended up with a real-time
// window ~12x too short.
func TestAlphaFor(t *testing.T) {
	fast := alphaFor(5)
	slow := alphaFor(60)
	if fast <= 0 || fast >= 1 {
		t.Fatalf("alphaFor(5) = %v, want in (0,1)", fast)
	}
	if slow <= 0 || slow >= 1 {
		t.Fatalf("alphaFor(60) = %v, want in (0,1)", slow)
	}
	if fast >= slow {
		t.Fatalf("alphaFor(5) = %v, want < alphaFor(60) = %v (shorter interval -> smaller alpha, so the time-based window stays ~constant)", fast, slow)
	}
	if alphaFor(0) != alphaFor(60) {
		t.Fatalf("alphaFor(0) = %v, want == alphaFor(60) = %v (non-positive interval falls back to 60)", alphaFor(0), alphaFor(60))
	}
	if alphaFor(-5) != alphaFor(60) {
		t.Fatalf("alphaFor(-5) = %v, want == alphaFor(60)", alphaFor(-5))
	}
}

func TestBaselineZScore(t *testing.T) {
	b := NewBaseline()
	for i := 0; i < 200; i++ {
		b.Observe("cpu", 10, 60) // stable around 10
	}
	if _, ready := b.Z("cpu", 10); !ready {
		t.Fatal("should be ready after 200 obs")
	}
	z, _ := b.Z("cpu", 60) // big spike
	if z < 3 {
		t.Fatalf("spike z = %v, expected >= 3", z)
	}
	zn, _ := b.Z("cpu", 10)
	if zn > 1 {
		t.Fatalf("norm z = %v, expected small", zn)
	}
}

func TestBaselineZScoreVaryingData(t *testing.T) {
	b := NewBaseline()
	vals := []float64{8, 9, 10, 11, 12}
	for i := 0; i < 400; i++ {
		b.Observe("cpu", vals[i%len(vals)], 60)
	}
	z, ready := b.Z("cpu", 10)
	if !ready {
		t.Fatal("should be ready after 400 obs")
	}
	if math.Abs(z) >= 2 {
		t.Fatalf("in-range z = %v, expected |z| < 2", z)
	}
	zo, _ := b.Z("cpu", 100)
	if zo < 3 {
		t.Fatalf("outlier z = %v, expected >= 3", zo)
	}
}

// TestBaselineMeanAccessor confirms Mean returns the tracked EWMA mean for a
// known key and (0, false) for a key that has never been observed.
func TestBaselineMeanAccessor(t *testing.T) {
	b := NewBaseline()
	if _, ok := b.Mean("cpu"); ok {
		t.Fatal("Mean of unknown key should be (0, false)")
	}
	for i := 0; i < 50; i++ {
		b.Observe("cpu", 43, 5)
	}
	mean, ok := b.Mean("cpu")
	if !ok {
		t.Fatal("Mean should be ready once a stat exists")
	}
	if math.Abs(mean-43) > 0.5 {
		t.Fatalf("Mean = %v, want ~43", mean)
	}
}

func TestBaselineNotReadyEarly(t *testing.T) {
	b := NewBaseline()
	b.Observe("mem", 50, 60)
	if _, ready := b.Z("mem", 90); ready {
		t.Fatal("should not be ready with 1 obs")
	}
}

// TestBaselinePerIntervalWindow feeds varying data at both the fast tier
// (5s) and the slow tier (60s) and asserts each metric independently
// becomes ready and produces sane, finite z-scores for in-range values and
// a large z-score for a genuine outlier -- regardless of its interval.
func TestBaselinePerIntervalWindow(t *testing.T) {
	b := NewBaseline()
	vals := []float64{8, 9, 10, 11, 12}
	for i := 0; i < 400; i++ {
		b.Observe("cpu", vals[i%len(vals)], 5)     // fast tier
		b.Observe("disk:/", vals[i%len(vals)], 60) // slow tier
	}

	for _, key := range []string{"cpu", "disk:/"} {
		z, ready := b.Z(key, 10)
		if !ready {
			t.Fatalf("%s: should be ready after 400 obs", key)
		}
		if math.IsInf(z, 0) || math.IsNaN(z) {
			t.Fatalf("%s: in-range z = %v, expected finite", key, z)
		}
		if math.Abs(z) >= 2 {
			t.Fatalf("%s: in-range z = %v, expected |z| < 2", key, z)
		}
		zo, ok := b.Z(key, 100)
		if !ok {
			t.Fatalf("%s: expected ready for outlier check", key)
		}
		if math.IsNaN(zo) {
			t.Fatalf("%s: outlier z = %v, expected finite/inf but not NaN", key, zo)
		}
		if zo < 3 {
			t.Fatalf("%s: outlier z = %v, expected >= 3", key, zo)
		}
	}

	// Different intervals must yield different stored alphas (smaller for
	// the shorter/fast-tier interval -- see TestAlphaFor for why).
	fastAlpha := b.Stats["cpu"].Alpha
	slowAlpha := b.Stats["disk:/"].Alpha
	if fastAlpha >= slowAlpha {
		t.Fatalf("fast-tier alpha %v should be < slow-tier alpha %v", fastAlpha, slowAlpha)
	}
}

// TestBaselineBackCompatZeroAlpha simulates a stat loaded from an old
// baseline.json (persisted before Alpha existed), which will unmarshal with
// Alpha==0. Since the stat already exists, Observe must not stomp on it (per
// spec, Alpha is only ever set when a stat is first created) -- instead both
// Observe and Z must treat a stored Alpha==0 as alphaFor(60) at use time.
func TestBaselineBackCompatZeroAlpha(t *testing.T) {
	b := NewBaseline()
	b.Stats["mem"] = &stat{Mean: 10, Count: 0, Alpha: 0} // as if loaded from pre-Alpha JSON
	for i := 0; i < 200; i++ {
		b.Observe("mem", 10, 5) // interval is irrelevant here: existing stat keeps its (legacy) Alpha
	}
	if b.Stats["mem"].Alpha != 0 {
		t.Fatalf("Alpha = %v, want unchanged 0 for a pre-existing stat (only set on create)", b.Stats["mem"].Alpha)
	}
	z, ready := b.Z("mem", 10)
	if !ready {
		t.Fatal("should be ready after 200 obs even with legacy zero Alpha")
	}
	if math.IsNaN(z) || math.IsInf(z, 0) {
		t.Fatalf("in-range z = %v, expected finite", z)
	}
	spikeZ, ok := b.Z("mem", 60)
	if !ok {
		t.Fatal("spike check should be ready")
	}
	if spikeZ < 3 {
		t.Fatalf("spike z = %v, expected >= 3 (fallback to alphaFor(60) must behave sanely)", spikeZ)
	}
}
