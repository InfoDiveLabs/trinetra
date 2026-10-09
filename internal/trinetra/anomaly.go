package trinetra

import (
	"encoding/json"
	"fmt"
	"math"
)

type ActiveAlert struct {
	Since  int64  `json:"since"`
	Reason string `json:"reason"`
	// Acked/AckedAt record a manual `trinetra alerts ack <key>`.
	Acked   bool  `json:"acked,omitempty"`
	AckedAt int64 `json:"acked_at,omitempty"`
	// Critical mirrors the firing Check's own Critical field (the severity of whatever
	// condition raised this alert), recorded at fire time so consumers of alerts.json.
	Critical bool `json:"critical,omitempty"`
}

type AlertState struct {
	Active map[string]ActiveAlert `json:"active"`
}

func NewAlertState() *AlertState { return &AlertState{Active: map[string]ActiveAlert{}} }

type Check struct {
	Key          string
	Value        float64
	Threshold    float64
	HasThreshold bool
	Critical     bool
	Interval     int    // seconds between samples for this metric's tier
	FireMsg      string // optional human fire message; overrides the numeric format in breach()
	RecoverMsg   string // optional human recover message; overrides the default "<key> back to normal"
}

type Event struct {
	Key      string
	Kind     string // "fire" | "recover"
	Text     string
	Critical bool
}

// Evaluate checks each of checks for a breach and fires/recovers its active state
// accordingly. baselineAlerts gates the baseline.
func (s *AlertState) Evaluate(checks []Check, b *Baseline, sigma, minPct float64, baselineAlerts bool, nowUnix int64) []Event {
	var events []Event
	for _, c := range checks {
		breach, reason := c.breach(b, sigma, minPct, baselineAlerts)
		_, active := s.Active[c.Key]
		switch {
		case breach && !active:
			s.Active[c.Key] = ActiveAlert{Since: nowUnix, Reason: reason, Critical: c.Critical}
			events = append(events, Event{Key: c.Key, Kind: "fire", Text: reason, Critical: c.Critical})
		case !breach && active:
			delete(s.Active, c.Key)
			recoverText := c.Key + " back to normal"
			if c.RecoverMsg != "" {
				recoverText = c.RecoverMsg
			}
			events = append(events, Event{Key: c.Key, Kind: "recover", Text: recoverText, Critical: c.Critical})
		}
		// feed baseline AFTER evaluating so a spike doesn't hide itself
		b.Observe(c.Key, c.Value, c.Interval)
	}
	return events
}

// Ack marks the active alert at key as acknowledged, recording nowUnix as AckedAt.
func (s *AlertState) Ack(key string, nowUnix int64) error {
	a, ok := s.Active[key]
	if !ok {
		return fmt.Errorf("no active alert for key %q", key)
	}
	a.Acked = true
	a.AckedAt = nowUnix
	s.Active[key] = a
	return nil
}

// MergeAckFromDisk reconciles the in-memory AlertState with ack flags that a CLI `alerts
// ack`/`unack` may have written to disk while the daemon was running.
func (s *AlertState) MergeAckFromDisk(path string, fs FileSource) {
	disk := LoadAlertState(path, fs)
	for key, mem := range s.Active {
		d, ok := disk.Active[key]
		if !ok {
			continue
		}
		mem.Acked = d.Acked
		mem.AckedAt = d.AckedAt
		s.Active[key] = mem
	}
}

// Unack clears a prior acknowledgement on the active alert at key.
func (s *AlertState) Unack(key string) error {
	a, ok := s.Active[key]
	if !ok {
		return fmt.Errorf("no active alert for key %q", key)
	}
	a.Acked = false
	a.AckedAt = 0
	s.Active[key] = a
	return nil
}

// meanFloor bounds the denominator of the minPct relative-deviation gate so a metric whose
// baseline mean sits near zero doesn't divide by (near) zero -- without it.
const meanFloor = 1.0

// breach reports whether c currently breaches, and its human reason text.
func (c Check) breach(b *Baseline, sigma, minPct float64, baselineAlerts bool) (bool, string) {
	if c.HasThreshold && c.Value >= c.Threshold {
		if c.FireMsg != "" {
			return true, c.FireMsg
		}
		return true, fmt.Sprintf("%s = %.1f ≥ threshold %.1f", c.Key, c.Value, c.Threshold)
	}
	if !baselineAlerts {
		return false, ""
	}
	if z, ready := b.Z(c.Key, c.Value); ready && math.Abs(z) >= sigma {
		// A metric can be many sigma from its mean while barely moving in absolute/relative terms
		// if its EWMA variance is underestimated.
		mean, _ := b.Mean(c.Key)
		denom := math.Abs(mean)
		if denom < meanFloor {
			denom = meanFloor
		}
		if math.Abs(c.Value-mean) < minPct*denom {
			return false, ""
		}
		return true, fmt.Sprintf("%s = %.1f is %.1fσ from baseline", c.Key, c.Value, z)
	}
	return false, ""
}

func (s *AlertState) Save(path string) error {
	bs, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, bs, 0o644)
}

func LoadAlertState(path string, fs FileSource) *AlertState {
	s := NewAlertState()
	bs, err := fs.Read(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(bs, s)
	if s.Active == nil {
		s.Active = map[string]ActiveAlert{}
	}
	return s
}
