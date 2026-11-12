package main

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
)

const certHorizonDays = 30

// Store holds the configured checks and their latest results for the views.
type Store struct {
	mu     sync.Mutex
	checks []Check
	res    map[string]Result
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{res: map[string]Result{}} }

// SetChecks replaces the check list and drops results of checks that are gone.
func (s *Store) SetChecks(cs []Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append([]Check(nil), cs...)
	keep := map[string]Result{}
	for _, c := range cs {
		if r, ok := s.res[c.Name]; ok {
			keep[c.Name] = r
		}
	}
	s.res = keep
}

// Record stores the latest result for c.
func (s *Store) Record(c Check, r Result) {
	s.mu.Lock()
	s.res[c.Name] = r
	s.mu.Unlock()
}

// Snapshot returns copies of the checks and results.
func (s *Store) Snapshot() ([]Check, map[string]Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := make(map[string]Result, len(s.res))
	for k, v := range s.res {
		res[k] = v
	}
	return append([]Check(nil), s.checks...), res
}

const emptyHint = "No checks configured. Add lines in the plugin settings, for example: http https://example.com"

func latencyText(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + " ms" }

// ChecksPage renders the nav page: a status list and a detail table.
func ChecksPage(checks []Check, res map[string]Result) []plugin.Widget {
	if len(checks) == 0 {
		return []plugin.Widget{pluginsdk.Text("Uptime", emptyHint)}
	}
	items := make([]plugin.StatusItem, 0, len(checks))
	rows := make([][]string, 0, len(checks))
	for _, c := range checks {
		r, ran := res[c.Name]
		item := plugin.StatusItem{Name: c.Name, State: "unknown", Detail: "waiting for first run"}
		row := []string{c.Name, string(c.Kind), c.Target, "never", "-", "-"}
		if ran {
			item.State = "down"
			item.Detail = r.Detail
			if r.OK {
				item.State = "up"
				item.Detail = latencyText(r.Latency)
			}
			row[3] = r.At.UTC().Format("2006-01-02 15:04:05 UTC")
			if r.OK {
				row[4] = latencyText(r.Latency)
			}
			if r.HasCert {
				row[5] = fmt.Sprintf("%.0f", r.CertDays)
			}
		}
		items = append(items, item)
		rows = append(rows, row)
	}
	return []plugin.Widget{
		pluginsdk.Status("Checks", items...),
		pluginsdk.Table("Details", []string{"Name", "Kind", "Target", "Last check", "Latency", "Cert days"}, rows),
	}
}

// Overview renders the dashboard panel: combined latency graph and the expiring-certs stat.
func Overview(checks []Check, res map[string]Result) []plugin.Widget {
	if len(checks) == 0 {
		return []plugin.Widget{pluginsdk.Text("Uptime overview", emptyHint)}
	}
	var series []plugin.SeriesRef
	for _, c := range checks {
		if len(series) == plugin.MaxSeriesPerChart {
			break
		}
		series = append(series, pluginsdk.Series("check."+c.Name+".latency_ms", c.Name))
	}
	n := 0
	for _, r := range res {
		if r.HasCert && r.CertDays < certHorizonDays {
			n++
		}
	}
	stat := pluginsdk.Stat(fmt.Sprintf("Certs expiring within %d days", certHorizonDays), strconv.Itoa(n), "")
	tone := "ok"
	if n > 0 {
		tone = "warn"
	}
	return []plugin.Widget{
		pluginsdk.Timeseries("Latency", "ms", series...),
		pluginsdk.StatTone(stat, tone),
	}
}
