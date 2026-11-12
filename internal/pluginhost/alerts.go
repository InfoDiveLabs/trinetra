package pluginhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const (
	maxAlertTitleRunes = 200
	maxAlertBodyRunes  = 4096
	alertKeyPrefix     = "plugin."
)

// AlertOut is the delivered alert shape handed to the core notifier queue.
type AlertOut struct {
	Key, Title, Body, Severity, Kind, Source string
	Time                                     int64
}

type bookEntry struct {
	Plugin   string `json:"plugin"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	Since    int64  `json:"since"`
	Acked    bool   `json:"acked,omitempty"`
	AckedAt  int64  `json:"acked_at,omitempty"`
}

type bookFile struct {
	Active map[string]bookEntry `json:"active"`
}

// AlertBook holds plugin alerts durably and hands fire/recover events to the core delivery path.
type AlertBook struct {
	path    string
	mu      sync.Mutex
	active  map[string]bookEntry
	deliver func(AlertOut)
	now     func() time.Time
}

// OpenAlertBook loads the book at path (missing or unreadable content starts empty).
func OpenAlertBook(path string, deliver func(AlertOut), now func() time.Time) (*AlertBook, error) {
	if now == nil {
		now = time.Now
	}
	if deliver == nil {
		deliver = func(AlertOut) {}
	}
	b := &AlertBook{path: path, active: map[string]bookEntry{}, deliver: deliver, now: now}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f bookFile
		if json.Unmarshal(raw, &f) == nil && f.Active != nil {
			b.active = f.Active
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return b, nil
}

func (b *AlertBook) persist() error {
	raw, err := json.Marshal(bookFile{Active: b.active})
	if err != nil {
		return err
	}
	return writeFileAtomic(b.path, raw, 0o600)
}

func alertSource(id string) string { return "plugin:" + id }

// Raise persists then delivers a fire; re-raising an active key with the same severity is a no-op.
func (b *AlertBook) Raise(id string, r plugin.AlertRequest) error {
	if !plugin.MetricPattern.MatchString(r.Key) {
		return fmt.Errorf("alert key %q is invalid", r.Key)
	}
	switch r.Severity {
	case "info", "warning", "critical":
	default:
		return fmt.Errorf("alert severity %q must be info, warning or critical", r.Severity)
	}
	if n := utf8.RuneCountInString(r.Title); n < 1 || n > maxAlertTitleRunes {
		return fmt.Errorf("alert title must be 1-%d characters", maxAlertTitleRunes)
	}
	if utf8.RuneCountInString(r.Body) > maxAlertBodyRunes {
		return fmt.Errorf("alert body is longer than %d characters", maxAlertBodyRunes)
	}
	key := alertKeyPrefix + id + "." + r.Key
	b.mu.Lock()
	defer b.mu.Unlock()
	prev, exists := b.active[key]
	if exists && prev.Severity == r.Severity {
		return nil
	}
	if !exists {
		n := 0
		for _, e := range b.active {
			if e.Plugin == id {
				n++
			}
		}
		if n >= plugin.MaxActiveAlertsPerPlugin {
			return fmt.Errorf("plugin has %d active alerts; resolve some first", n)
		}
	}
	now := b.now().Unix()
	e := bookEntry{Plugin: id, Severity: r.Severity, Title: r.Title, Body: r.Body, Since: now}
	if exists {
		e.Acked, e.AckedAt = false, 0
	}
	b.active[key] = e
	if err := b.persist(); err != nil {
		if exists {
			b.active[key] = prev
		} else {
			delete(b.active, key)
		}
		return err
	}
	b.deliver(AlertOut{Key: key, Title: e.Title, Body: e.Body, Severity: e.Severity, Kind: "fire", Source: alertSource(id), Time: now})
	return nil
}

// Resolve persists then delivers a recover; an unknown key is a no-op.
func (b *AlertBook) Resolve(id string, r plugin.ResolveRequest) error {
	key := alertKeyPrefix + id + "." + r.Key
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolveLocked(id, key, r.Body)
}

func (b *AlertBook) resolveLocked(id, key, body string) error {
	e, ok := b.active[key]
	if !ok || e.Plugin != id {
		return nil
	}
	delete(b.active, key)
	if err := b.persist(); err != nil {
		b.active[key] = e
		return err
	}
	if utf8.RuneCountInString(body) > maxAlertBodyRunes {
		body = string([]rune(body)[:maxAlertBodyRunes])
	}
	b.deliver(AlertOut{Key: key, Title: e.Title, Body: body, Severity: e.Severity, Kind: "recover", Source: alertSource(id), Time: b.now().Unix()})
	return nil
}

// ResolveAll resolves every active alert of id, delivering a recover for each.
func (b *AlertBook) ResolveAll(id, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keys []string
	for k, e := range b.active {
		if e.Plugin == id {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		_ = b.resolveLocked(id, k, reason)
	}
}

// Active lists active plugin alerts sorted by key.
func (b *AlertBook) Active() []core.AlertRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]core.AlertRecord, 0, len(b.active))
	for k, e := range b.active {
		out = append(out, core.AlertRecord{Key: k, Severity: e.Severity, Source: alertSource(e.Plugin), Time: e.Since,
			Acked: e.Acked, AckedAt: e.AckedAt, Title: e.Title})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Ack marks an active plugin alert acknowledged.
func (b *AlertBook) Ack(key string, now int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.active[key]
	if !ok {
		return fmt.Errorf("no active alert %q", key)
	}
	prev := e
	e.Acked, e.AckedAt = true, now
	b.active[key] = e
	if err := b.persist(); err != nil {
		b.active[key] = prev
		return err
	}
	return nil
}

// Unack clears an acknowledgement.
func (b *AlertBook) Unack(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.active[key]
	if !ok {
		return fmt.Errorf("no active alert %q", key)
	}
	prev := e
	e.Acked, e.AckedAt = false, 0
	b.active[key] = e
	if err := b.persist(); err != nil {
		b.active[key] = prev
		return err
	}
	return nil
}

// Owns reports whether key belongs to a plugin alert.
func (b *AlertBook) Owns(key string) bool { return strings.HasPrefix(key, alertKeyPrefix) }
