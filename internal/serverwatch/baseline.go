package serverwatch

import (
	"encoding/json"
	"math"
)

// stat tracks an exponentially weighted mean and variance (West's method).
// Alpha is derived per-metric from its sampling interval (see alphaFor) so
// every metric's effective baseline window is ~7 days, regardless of
// whether it's sampled at the fast tier (5s) or slow tier (60s).
type stat struct {
	Mean  float64 `json:"mean"`
	Var   float64 `json:"var"`
	Count int     `json:"count"`
	Alpha float64 `json:"alpha"`
}

const baselineWindowSeconds = 7 * 86400 // ~7-day baseline window
const baselineReady = 30

// alphaFor derives the EW smoothing factor for a metric sampled every
// intervalSec seconds, targeting an effective window of ~7 days:
// alpha = 2/(N+1), N = window / intervalSec. A shorter interval means more
// samples fall inside the same 7-day window, so N is larger and alpha is
// smaller -- that's what keeps the real-time (wall-clock) window equal
// across the fast (5s) and slow (60s) tiers, which is exactly what a single
// fixed alpha got wrong. A non-positive interval is treated as the
// historical default of 60s (also used as the back-compat fallback for
// stats persisted before Alpha existed).
func alphaFor(intervalSec int) float64 {
	if intervalSec <= 0 {
		intervalSec = 60
	}
	n := float64(baselineWindowSeconds) / float64(intervalSec)
	return 2 / (n + 1)
}

type Baseline struct {
	Stats map[string]*stat `json:"stats"`
}

func NewBaseline() *Baseline { return &Baseline{Stats: map[string]*stat{}} }

func (b *Baseline) Observe(key string, v float64, intervalSec int) {
	s := b.Stats[key]
	if s == nil {
		s = &stat{Mean: v, Alpha: alphaFor(intervalSec)}
		b.Stats[key] = s
	}
	alpha := s.Alpha
	if alpha == 0 {
		alpha = alphaFor(60) // back-compat: pre-existing stat loaded without Alpha
	}
	s.Count++
	diff := v - s.Mean
	incr := alpha * diff
	s.Mean += incr
	s.Var = (1 - alpha) * (s.Var + diff*incr)
}

// Mean returns the tracked EWMA mean for key and whether a stat exists for
// it yet (unlike Z, this does NOT require baselineReady observations --
// callers needing the relative-deviation gate want the current running
// mean as soon as one exists, not only once the baseline is "ready").
func (b *Baseline) Mean(key string) (float64, bool) {
	s := b.Stats[key]
	if s == nil {
		return 0, false
	}
	return s.Mean, true
}

func (b *Baseline) Z(key string, v float64) (float64, bool) {
	s := b.Stats[key]
	if s == nil || s.Count < baselineReady {
		return 0, false
	}
	alpha := s.Alpha
	if alpha == 0 {
		alpha = alphaFor(60) // back-compat: pre-existing stat loaded without Alpha
	}
	factor := 1 - math.Pow(1-alpha, float64(s.Count))
	if factor < 1e-12 {
		return 0, false // not enough weight accumulated yet
	}
	corrected := s.Var / factor
	sd := math.Sqrt(corrected)
	if sd < 1e-9 {
		if v == s.Mean {
			return 0, true
		}
		return math.Inf(1), true
	}
	return (v - s.Mean) / sd, true
}

func (b *Baseline) Save(path string) error {
	bs, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, bs, 0o644)
}

func (b *Baseline) LoadFrom(path string, fs FileSource) {
	bs, err := fs.Read(path)
	if err != nil {
		return
	}
	b.Stats = map[string]*stat{}
	_ = json.Unmarshal(bs, b)
	if b.Stats == nil {
		b.Stats = map[string]*stat{}
	}
}
