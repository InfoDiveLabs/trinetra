// Package serverwatch: alertlog.go implements the alert event log -- an
// append-only JSONL history of every alert notification the daemon has
// dispatched (fired/recovered, and which channels actually delivered it).
// This is deliberately separate from AlertState (alerts.json, the current
// *active* alerts) and from the downtime/sample stores: it exists purely so
// a human can later answer "was I notified about X, and did it get through?"
package serverwatch

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// Delivery records the outcome of dispatching one AlertEvent to one channel.
type Delivery struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Err     string `json:"err,omitempty"`
}

// AlertEvent is one line in the alert log: a record that an Alert was
// dispatched, plus which channels it was actually delivered to.
type AlertEvent struct {
	Time      int64      `json:"time"`
	Key       string     `json:"key"`
	Title     string     `json:"title"`
	Severity  string     `json:"severity"`
	Kind      string     `json:"kind"` // "fire" | "recover"
	Source    string     `json:"source"`
	Delivered []Delivery `json:"delivered,omitempty"`
}

// AlertLog is a thin wrapper around a single JSONL file holding AlertEvents.
type AlertLog struct {
	path string
}

// NewAlertLog returns an AlertLog backed by path. The file is created lazily
// on first append; it is fine for path not to exist yet.
func NewAlertLog(path string) *AlertLog { return &AlertLog{path: path} }

// AppendAlertEvent appends ev to the log.
func (l *AlertLog) AppendAlertEvent(ev AlertEvent) error {
	return appendJSONL(l.path, ev)
}

// AlertEventsSince returns every AlertEvent with Time >= sinceUnix, in
// on-disk (chronological append) order. A missing log file is not an error:
// it simply yields no events. Corrupt/malformed lines are skipped rather
// than failing the whole read, mirroring Store.DownSince.
func (l *AlertLog) AlertEventsSince(sinceUnix int64) ([]AlertEvent, error) {
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []AlertEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev AlertEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Time >= sinceUnix {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}

// PruneAlertLog rewrites the log keeping only events with Time >= beforeUnix,
// dropping everything older. Intended to be called periodically (e.g. on the
// daemon's slow tick) to bound the log to roughly the caller's chosen
// retention window; it is a no-op (not an error) if the log doesn't exist yet.
func (l *AlertLog) PruneAlertLog(beforeUnix int64) error {
	if _, err := os.Stat(l.path); os.IsNotExist(err) {
		return nil // nothing to prune; don't create an empty file
	}
	evs, err := l.AlertEventsSince(beforeUnix)
	if err != nil {
		return err
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	for _, e := range evs {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return writeFileAtomic(l.path, []byte(buf.String()), 0o644)
}

// deliveriesFrom converts the Dispatcher's []DeliveryResult into the
// []Delivery shape persisted in the alert log: OK is true iff Err was nil,
// and Err carries Err.Error() otherwise. Pure and side-effect free so it's
// trivially unit-testable apart from any actual dispatch.
func deliveriesFrom(results []DeliveryResult) []Delivery {
	if len(results) == 0 {
		return nil
	}
	out := make([]Delivery, len(results))
	for i, r := range results {
		d := Delivery{Channel: r.Channel, OK: r.Err == nil}
		if r.Err != nil {
			d.Err = r.Err.Error()
		}
		out[i] = d
	}
	return out
}
