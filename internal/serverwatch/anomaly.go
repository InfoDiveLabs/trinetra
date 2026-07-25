package serverwatch

import (
	"encoding/json"
	"fmt"
	"math"
)

type ActiveAlert struct {
	Since  int64  `json:"since"`
	Reason string `json:"reason"`
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
}

type Event struct {
	Key      string
	Kind     string // "fire" | "recover"
	Text     string
	Critical bool
}

func (s *AlertState) Evaluate(checks []Check, b *Baseline, sigma float64, nowUnix int64) []Event {
	var events []Event
	for _, c := range checks {
		breach, reason := c.breach(b, sigma)
		_, active := s.Active[c.Key]
		switch {
		case breach && !active:
			s.Active[c.Key] = ActiveAlert{Since: nowUnix, Reason: reason}
			events = append(events, Event{Key: c.Key, Kind: "fire", Text: reason, Critical: c.Critical})
		case !breach && active:
			delete(s.Active, c.Key)
			events = append(events, Event{Key: c.Key, Kind: "recover", Text: c.Key + " back to normal", Critical: c.Critical})
		}
		// feed baseline AFTER evaluating so a spike doesn't hide itself
		b.Observe(c.Key, c.Value)
	}
	return events
}

func (c Check) breach(b *Baseline, sigma float64) (bool, string) {
	if c.HasThreshold && c.Value >= c.Threshold {
		return true, fmt.Sprintf("%s = %.1f ≥ threshold %.1f", c.Key, c.Value, c.Threshold)
	}
	if z, ready := b.Z(c.Key, c.Value); ready && math.Abs(z) >= sigma {
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
