package main

import (
	"fmt"
	"sync"
)

// Op is an alert call the rules want made.
type Op struct {
	Resolve  bool
	Key      string
	Severity string
	Title    string
	Body     string
}

type ruleState struct {
	fails int
	down  bool
	cert  bool
}

// Rules tracks per-check alert state and decides when to raise or resolve.
type Rules struct {
	mu sync.Mutex
	st map[string]*ruleState
}

// NewRules returns empty rule state.
func NewRules() *Rules { return &Rules{st: map[string]*ruleState{}} }

func downKey(name string) string { return "check." + name + ".down" }
func certKey(name string) string { return "cert." + name + ".expiring" }

// Observe folds one result into the state and returns the alert ops it implies.
func (r *Rules) Observe(c Check, res Result, downAfter, certWarnDays int) []Op {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.st[c.Name]
	if s == nil {
		s = &ruleState{}
		r.st[c.Name] = s
	}
	var ops []Op
	if res.OK {
		s.fails = 0
		if s.down {
			s.down = false
			ops = append(ops, Op{Resolve: true, Key: downKey(c.Name), Body: fmt.Sprintf("%s is back up", c.Name)})
		}
	} else {
		s.fails++
		if !s.down && s.fails >= downAfter {
			s.down = true
			ops = append(ops, Op{Key: downKey(c.Name), Severity: "critical",
				Title: fmt.Sprintf("%s is down", c.Name),
				Body:  fmt.Sprintf("%s %s failed %d runs in a row: %s", c.Kind, c.Target, s.fails, res.Detail)})
		}
	}
	if res.HasCert {
		expiring := res.CertDays < float64(certWarnDays)
		switch {
		case expiring && !s.cert:
			s.cert = true
			ops = append(ops, Op{Key: certKey(c.Name), Severity: "warning",
				Title: fmt.Sprintf("Certificate for %s expires in %s", c.Name, daysText(res.CertDays)),
				Body:  fmt.Sprintf("%s certificate expires in %s (alert threshold %d days)", c.Target, daysText(res.CertDays), certWarnDays)})
		case !expiring && s.cert:
			s.cert = false
			ops = append(ops, Op{Resolve: true, Key: certKey(c.Name), Body: fmt.Sprintf("certificate for %s renewed", c.Name)})
		}
	}
	return ops
}

func daysText(d float64) string {
	if d < 0 {
		return fmt.Sprintf("%.0f days ago (expired)", -d)
	}
	return fmt.Sprintf("%.0f days", d)
}

// Forget drops a check's state and returns resolves for its active alerts.
func (r *Rules) Forget(name string) []Op {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.st[name]
	if s == nil {
		return nil
	}
	delete(r.st, name)
	var ops []Op
	if s.down {
		ops = append(ops, Op{Resolve: true, Key: downKey(name), Body: "check removed"})
	}
	if s.cert {
		ops = append(ops, Op{Resolve: true, Key: certKey(name), Body: "check removed"})
	}
	return ops
}

// Names lists checks with state.
func (r *Rules) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.st))
	for n := range r.st {
		out = append(out, n)
	}
	return out
}

// Revert undoes the state change behind an op whose host call failed, so the next run retries it.
func (r *Rules) Revert(op Op) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, s := range r.st {
		switch op.Key {
		case downKey(name):
			s.down = op.Resolve
		case certKey(name):
			s.cert = op.Resolve
		}
	}
}
