// Package trinetra: alertlog.go implements the alert event log -- an
// append-only JSONL history of every alert notification the daemon has
// dispatched (fired/recovered, and which channels actually delivered it).
// This is deliberately separate from AlertState (alerts.json, the current
// *active* alerts) and from the downtime/sample stores: it exists purely so
// a human can later answer "was I notified about X, and did it get through?"
package trinetra

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

	// RoutedToMaster is true when a child held a valid lease for this alert at enqueue time,
	// so it was NOT enqueued for local delivery.
	RoutedToMaster bool `json:"routed_to_master,omitempty"`
	// DeliveredLocally is true on the second AlertEvent a child's handoff records when a
	// routed alert's receipt never arrived.
	DeliveredLocally bool `json:"delivered_locally,omitempty"`
	// FiredAt is the original fire (or recover) event's unix time.
	FiredAt int64 `json:"fired_at,omitempty"`
}

// AlertLog is a thin wrapper around a single JSONL file holding AlertEvents.
type AlertLog struct {
	path string
	mu   sync.Mutex
	tee  atomic.Pointer[func(AlertEvent)]
}

// NewAlertLog returns an AlertLog backed by path.
func NewAlertLog(path string) *AlertLog { return &AlertLog{path: path} }

// SetTee installs f to receive every event after it is appended (the fleet
// child ships alert history to the master). nil clears it.
func (l *AlertLog) SetTee(f func(AlertEvent)) {
	if f == nil {
		l.tee.Store(nil)
		return
	}
	l.tee.Store(&f)
}

// AppendAlertEvent appends ev to the log.
func (l *AlertLog) AppendAlertEvent(ev AlertEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := appendJSONL(l.path, ev); err != nil {
		return err
	}
	if f := l.tee.Load(); f != nil {
		(*f)(ev)
	}
	return nil
}

// AlertEventsSince returns every AlertEvent with Time >= sinceUnix, in on-disk
// (chronological append) order.
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

// PruneAlertLog rewrites the log keeping only events with Time >= beforeUnix, dropping
// everything older.
func (l *AlertLog) PruneAlertLog(beforeUnix int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
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

// deliveriesFrom converts the Dispatcher's []DeliveryResult into the []Delivery shape
// persisted in the alert log: OK is true iff Err was nil.
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
