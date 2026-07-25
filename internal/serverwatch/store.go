package serverwatch

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Store struct {
	dir   string
	clock Clock
}

func NewStore(dir string, clock Clock) *Store { return &Store{dir: dir, clock: clock} }

type Sample struct {
	TS      int64              `json:"ts"`
	CPU     float64            `json:"cpu"`
	MemPct  float64            `json:"mem_pct"`
	SwapPct float64            `json:"swap_pct"`
	Load1   float64            `json:"load1"`
	TempC   float64            `json:"temp_c"`
	Disks   map[string]float64 `json:"disks,omitempty"`
}

func (s *Store) samplesDir() string     { return filepath.Join(s.dir, "samples") }
func (s *Store) downPath() string       { return filepath.Join(s.dir, "downtime.jsonl") }
func (s *Store) HeartbeatPath() string  { return filepath.Join(s.dir, "heartbeat") }
func (s *Store) BaselinePath() string   { return filepath.Join(s.dir, "baseline.json") }
func (s *Store) AlertStatePath() string { return filepath.Join(s.dir, "alerts.json") }
func (s *Store) AlertLogPath() string   { return filepath.Join(s.dir, "alertlog.jsonl") }

func (s *Store) AppendSample(smp Sample) error {
	if err := os.MkdirAll(s.samplesDir(), 0o755); err != nil {
		return err
	}
	day := time.Unix(smp.TS, 0).UTC().Format("2006-01-02")
	return appendJSONL(filepath.Join(s.samplesDir(), day+".jsonl"), smp)
}

func (s *Store) PruneOlderThan(days int) error {
	cut := s.clock.Now().AddDate(0, 0, -days)
	files, _ := filepath.Glob(filepath.Join(s.samplesDir(), "*.jsonl"))
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		d, err := time.Parse("2006-01-02", base)
		if err != nil {
			continue
		}
		if d.Before(cut.Truncate(24 * time.Hour)) {
			_ = os.Remove(f)
		}
	}
	return s.pruneDown(cut.Unix())
}

func (s *Store) pruneDown(cutUnix int64) error {
	evs, err := s.DownSince(cutUnix)
	if err != nil {
		return err
	}
	// rewrite file with only kept events
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	for _, e := range evs {
		_ = enc.Encode(e)
	}
	return writeFileAtomic(s.downPath(), []byte(buf.String()), 0o644)
}

func (s *Store) AppendDown(ev DownEvent) error { return appendJSONL(s.downPath(), ev) }

func (s *Store) DownSince(sinceUnix int64) ([]DownEvent, error) {
	f, err := os.Open(s.downPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []DownEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e DownEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.End >= sinceUnix {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

func (s *Store) SamplesSince(sinceUnix int64) ([]Sample, error) {
	files, _ := filepath.Glob(filepath.Join(s.samplesDir(), "*.jsonl"))
	sort.Strings(files)
	var out []Sample
	for _, f := range files {
		// skip whole files whose day ends before the cutoff
		base := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if d, err := time.Parse("2006-01-02", base); err == nil {
			if d.Add(24*time.Hour).Unix() < sinceUnix {
				continue
			}
		}
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var smp Sample
			if json.Unmarshal([]byte(line), &smp) == nil && smp.TS >= sinceUnix {
				out = append(out, smp)
			}
		}
		fh.Close()
	}
	return out, nil
}

func (s *Store) WriteStatus(v any) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "status.json"), b, 0o644)
}

func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}
