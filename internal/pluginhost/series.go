package pluginhost

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const maxRejectedListed = 20

type seriesFile struct {
	Names []string `json:"names"`
}

// SeriesGate validates and namespaces plugin metrics and enforces the per-plugin series cap.
type SeriesGate struct {
	dir      string
	cap      int
	submit   func(int64, map[string]float64) bool
	now      func() time.Time
	mu       sync.Mutex
	known    map[string]map[string]bool
	rejected map[string]int64
}

// NewSeriesGate returns a gate persisting series names under stateDir/plugins/<id>/series.json.
func NewSeriesGate(stateDir string, submit func(ts int64, ms map[string]float64) bool, now func() time.Time) *SeriesGate {
	if now == nil {
		now = time.Now
	}
	return &SeriesGate{
		dir: filepath.Join(stateDir, "plugins"), cap: plugin.DefaultSeriesCap, submit: submit, now: now,
		known: map[string]map[string]bool{}, rejected: map[string]int64{},
	}
}

func (g *SeriesGate) path(id string) string { return filepath.Join(g.dir, id, "series.json") }

func (g *SeriesGate) load(id string) map[string]bool {
	if k, ok := g.known[id]; ok {
		return k
	}
	k := map[string]bool{}
	if b, err := os.ReadFile(g.path(id)); err == nil {
		var f seriesFile
		if json.Unmarshal(b, &f) == nil {
			for _, n := range f.Names {
				k[n] = true
			}
		}
	}
	g.known[id] = k
	return k
}

func (g *SeriesGate) persist(id string, k map[string]bool) {
	names := make([]string, 0, len(k))
	for n := range k {
		names = append(names, n)
	}
	sort.Strings(names)
	b, _ := json.Marshal(seriesFile{Names: names})
	_ = writeFileAtomic(g.path(id), b, 0o600)
}

func sortedNames(m map[string]float64) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func capList(names []string) []string {
	if len(names) > maxRejectedListed {
		return names[:maxRejectedListed]
	}
	return names
}

// Publish namespaces, validates, enforces the per-plugin cap and submits without blocking.
func (g *SeriesGate) Publish(id string, r plugin.PublishRequest) plugin.PublishResponse {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	ts := r.TS
	if ts == 0 {
		ts = now.Unix()
	}
	if len(r.Metrics) > plugin.MaxPublishPerCall || ts < now.Add(-10*time.Minute).Unix() || ts > now.Add(time.Minute).Unix() {
		g.rejected[id] += int64(len(r.Metrics))
		return plugin.PublishResponse{Rejected: capList(sortedNames(r.Metrics))}
	}
	known := g.load(id)
	accepted := map[string]float64{}
	var rejected, fresh []string
	for _, name := range sortedNames(r.Metrics) {
		v := r.Metrics[name]
		if !plugin.MetricPattern.MatchString(name) || math.IsNaN(v) || math.IsInf(v, 0) {
			rejected = append(rejected, name)
			continue
		}
		if !known[name] {
			if len(known)+len(fresh) >= g.cap {
				rejected = append(rejected, name)
				continue
			}
			fresh = append(fresh, name)
		}
		accepted["plugin."+id+"."+name] = v
	}
	g.rejected[id] += int64(len(rejected))
	resp := plugin.PublishResponse{Rejected: capList(rejected)}
	if len(accepted) == 0 {
		return resp
	}
	if g.submit == nil || !g.submit(ts, accepted) {
		resp.Dropped = true
		return resp
	}
	resp.Accepted = len(accepted)
	if len(fresh) > 0 {
		for _, n := range fresh {
			known[n] = true
		}
		g.persist(id, known)
	}
	return resp
}

// Count returns how many distinct series id has published.
func (g *SeriesGate) Count(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.load(id))
}

// Rejected returns how many points from id have been rejected.
func (g *SeriesGate) Rejected(id string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rejected[id]
}

// Forget drops id's persisted and in-memory series state.
func (g *SeriesGate) Forget(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.known, id)
	delete(g.rejected, id)
	_ = os.Remove(g.path(id))
}
