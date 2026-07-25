package serverwatch

import (
	"encoding/json"
	"math"
)

// stat tracks an exponentially weighted mean and variance (West's method).
// alpha ~ 2/(N+1); N≈10080 (7d @60s) -> alpha ≈ 0.000198.
type stat struct {
	Mean  float64 `json:"mean"`
	Var   float64 `json:"var"`
	Count int     `json:"count"`
}

const baselineAlpha = 0.000198
const baselineReady = 30

type Baseline struct {
	Stats map[string]*stat `json:"stats"`
}

func NewBaseline() *Baseline { return &Baseline{Stats: map[string]*stat{}} }

func (b *Baseline) Observe(key string, v float64) {
	s := b.Stats[key]
	if s == nil {
		s = &stat{Mean: v}
		b.Stats[key] = s
	}
	s.Count++
	diff := v - s.Mean
	incr := baselineAlpha * diff
	s.Mean += incr
	s.Var = (1 - baselineAlpha) * (s.Var + diff*incr)
}

func (b *Baseline) Z(key string, v float64) (float64, bool) {
	s := b.Stats[key]
	if s == nil || s.Count < baselineReady {
		return 0, false
	}
	factor := 1 - math.Pow(1-baselineAlpha, float64(s.Count))
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
