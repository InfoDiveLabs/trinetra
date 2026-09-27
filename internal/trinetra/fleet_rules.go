// Package trinetra: fleet_rules.go is the master's aggregate-rule engine
// (spec 6/task 7): a small, hand-written grammar (NOT PromQL) parsed by
// parseRuleExpr, evaluated every ruleTickInterval by fleetAlertEngine.TickRules
// against the same data sources fleet_provider.go's Nodes() and the child's
// own gap-filler already use -- the replica sink's latest snapshot
// (replicaSink.LiveOf) and 1m series (replicaNode.store.Query), the liveness
// tracker's state, and the registry's tags/LastSeen (see
// fleet-phase2-map.md section 8).
//
// Grammar (exactly as specified, verbatim from the task brief):
//
//	count(<sel>, <metric> <op> <num>) <op> <int> for <dur>
//	avg|max|min(<sel>, <metric>) <op> <num> for <dur>
//	online(<sel>) <op> <int> for <dur>
//	absent(<sel>, <dur>)
//
// <sel> is one of tag:<t>, node:<glob> or all. Metrics: cpu, mem, swap, disk
// (worst mount), load1, temp. Operators: > >= < <= == !=. `for` is required
// except for absent, and its duration has a 1m minimum. A rule's "for"
// sustain (and absent's blind window) is tracked purely in memory
// (fleetRuleEvaluator.states / .started): a master restart always starts
// both over, exactly as global-constraints documents for every other
// engine timer -- the orphan check (masterLoop.stillActive's
// "fleet:rule:<name>" case) is what reconciles a stale open incident after
// that, not this in-memory state.
package trinetra

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// ruleTickInterval is how often TickRules actually evaluates rules (task 7:
// "every 30s"), self-gated inside TickRules exactly like TickSilences's own
// silencePushInterval gate -- masterLoop.tick calls TickRules every 5s (its
// own tick cadence) and lets the gate decide whether this call does
// anything.
const ruleTickInterval = 30 * time.Second

// ---- grammar: tokenizer -----------------------------------------------

// ruleParseError is parseRuleExpr's error type: every error names the
// position (byte offset into the original expression string) it was found
// at, per the task-7 ruling ("errors name the position:
// \"expected ',' at 12\"").
type ruleParseError struct {
	Pos int
	Msg string
}

func (e *ruleParseError) Error() string { return fmt.Sprintf("%s at %d", e.Msg, e.Pos) }

func perr(pos int, format string, args ...any) *ruleParseError {
	return &ruleParseError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

type ruleTokenKind int

const (
	tokWord ruleTokenKind = iota
	tokOp
	tokLParen
	tokRParen
	tokComma
	tokEOF
)

type ruleToken struct {
	kind ruleTokenKind
	text string
	pos  int
}

// ruleLexer is a small hand-written scanner (task-7 ruling: "no
// regexp-only parsing"): every token is either punctuation ('(', ')', ','),
// a comparison operator (> >= < <= == !=), or a "word" -- a run of
// characters a selector, metric name, number, integer, duration or the
// `for` keyword can all be made of (letters, digits, and the handful of
// symbols a tag/glob/duration/number ever contains: ':', '_', '-', '.',
// '*', '?', '[', ']', '/'). Operator characters ('>', '<', '=', '!') are
// deliberately excluded from that set so a word never swallows the
// comparison that follows it.
type ruleLexer struct {
	s   string
	pos int
}

func newRuleLexer(s string) *ruleLexer { return &ruleLexer{s: s} }

func isRuleWordChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case ':', '_', '-', '.', '*', '?', '[', ']', '/':
		return true
	}
	return false
}

func (lx *ruleLexer) skipSpace() {
	for lx.pos < len(lx.s) && (lx.s[lx.pos] == ' ' || lx.s[lx.pos] == '\t') {
		lx.pos++
	}
}

func (lx *ruleLexer) next() (ruleToken, error) {
	lx.skipSpace()
	start := lx.pos
	if lx.pos >= len(lx.s) {
		return ruleToken{kind: tokEOF, pos: start}, nil
	}
	c := lx.s[lx.pos]
	switch {
	case c == '(':
		lx.pos++
		return ruleToken{kind: tokLParen, text: "(", pos: start}, nil
	case c == ')':
		lx.pos++
		return ruleToken{kind: tokRParen, text: ")", pos: start}, nil
	case c == ',':
		lx.pos++
		return ruleToken{kind: tokComma, text: ",", pos: start}, nil
	case c == '>' || c == '<':
		lx.pos++
		text := string(c)
		if lx.pos < len(lx.s) && lx.s[lx.pos] == '=' {
			text += "="
			lx.pos++
		}
		return ruleToken{kind: tokOp, text: text, pos: start}, nil
	case c == '=' || c == '!':
		lx.pos++
		if lx.pos >= len(lx.s) || lx.s[lx.pos] != '=' {
			return ruleToken{}, perr(start, "expected %q", string(c)+"=")
		}
		lx.pos++
		return ruleToken{kind: tokOp, text: string(c) + "=", pos: start}, nil
	case isRuleWordChar(c):
		for lx.pos < len(lx.s) && isRuleWordChar(lx.s[lx.pos]) {
			lx.pos++
		}
		return ruleToken{kind: tokWord, text: lx.s[start:lx.pos], pos: start}, nil
	}
	return ruleToken{}, perr(start, "unexpected character %q", string(c))
}

// ---- grammar: AST --------------------------------------------------------

// ruleSelector is <sel>: tag:<t>, node:<glob> (an exact node id or a
// path.Match glob against the node's display name, exactly Matcher.Node's
// own semantics) or all.
type ruleSelector struct {
	Kind  string // "tag", "node", "all"
	Value string // the tag name, or the node id-or-glob; "" for "all"
}

// ruleExpr is one parsed aggregate-rule expression. Not every field is used
// by every Kind: Metric/InnerOp/InnerNum are count-only; AbsentFor is
// absent-only; ForDur is every kind except absent (which has no `for`
// clause at all -- see forDuration).
type ruleExpr struct {
	Kind      string // "count", "avg", "max", "min", "online", "absent"
	Sel       ruleSelector
	Metric    string
	InnerOp   string
	InnerNum  float64
	OuterOp   string
	OuterNum  float64
	ForDur    time.Duration
	AbsentFor time.Duration
}

// forDuration is the sustain window a rule's condition must hold
// continuously before it fires: ForDur for every kind but absent, which has
// none (absent's own "for D straight" is baked into evaluating AbsentFor
// directly against LastSeen -- see evalAbsent) -- so it fires on the very
// first tick its condition evaluates true, exactly like a `for 0s` rule
// would.
func (e *ruleExpr) forDuration() time.Duration {
	if e.Kind == "absent" {
		return 0
	}
	return e.ForDur
}

// ruleMetrics are the only metric names a count/avg/max/min rule may name
// (task-7 ruling); disk is the WORST mount, not a specific one.
var ruleMetrics = map[string]bool{
	"cpu": true, "mem": true, "swap": true, "disk": true, "load1": true, "temp": true,
}

// ---- grammar: parser -----------------------------------------------------

type ruleParser struct {
	lx  *ruleLexer
	tok ruleToken
}

func newRuleParser(s string) (*ruleParser, error) {
	p := &ruleParser{lx: newRuleLexer(s)}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *ruleParser) advance() error {
	t, err := p.lx.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *ruleParser) expectKind(kind ruleTokenKind, human string) error {
	if p.tok.kind != kind {
		return perr(p.tok.pos, "expected %s", human)
	}
	return p.advance()
}

func (p *ruleParser) expectWord(word string) error {
	if p.tok.kind != tokWord || p.tok.text != word {
		return perr(p.tok.pos, "expected %q", word)
	}
	return p.advance()
}

func (p *ruleParser) parseSelector() (ruleSelector, error) {
	if p.tok.kind != tokWord {
		return ruleSelector{}, perr(p.tok.pos, "expected a selector (tag:<t>, node:<glob> or all)")
	}
	text, pos := p.tok.text, p.tok.pos
	if err := p.advance(); err != nil {
		return ruleSelector{}, err
	}
	if text == "all" {
		return ruleSelector{Kind: "all"}, nil
	}
	if rest, ok := strings.CutPrefix(text, "tag:"); ok {
		if rest == "" {
			return ruleSelector{}, perr(pos, "empty tag selector")
		}
		return ruleSelector{Kind: "tag", Value: rest}, nil
	}
	if rest, ok := strings.CutPrefix(text, "node:"); ok {
		if rest == "" || !validGlob(rest) {
			return ruleSelector{}, perr(pos, "invalid node selector %q", text)
		}
		return ruleSelector{Kind: "node", Value: rest}, nil
	}
	return ruleSelector{}, perr(pos, "invalid selector %q", text)
}

func (p *ruleParser) parseMetric() (string, error) {
	if p.tok.kind != tokWord {
		return "", perr(p.tok.pos, "expected a metric")
	}
	text, pos := p.tok.text, p.tok.pos
	if !ruleMetrics[text] {
		return "", perr(pos, "unknown metric %q", text)
	}
	if err := p.advance(); err != nil {
		return "", err
	}
	return text, nil
}

func (p *ruleParser) parseOp() (string, error) {
	if p.tok.kind != tokOp {
		return "", perr(p.tok.pos, "expected an operator (> >= < <= == !=)")
	}
	text := p.tok.text
	if err := p.advance(); err != nil {
		return "", err
	}
	return text, nil
}

func (p *ruleParser) parseNumber() (float64, error) {
	if p.tok.kind != tokWord {
		return 0, perr(p.tok.pos, "expected a number")
	}
	text, pos := p.tok.text, p.tok.pos
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, perr(pos, "invalid number %q", text)
	}
	if err := p.advance(); err != nil {
		return 0, err
	}
	return v, nil
}

func (p *ruleParser) parseInt() (float64, error) {
	if p.tok.kind != tokWord {
		return 0, perr(p.tok.pos, "expected an integer")
	}
	text, pos := p.tok.text, p.tok.pos
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, perr(pos, "invalid integer %q", text)
	}
	if err := p.advance(); err != nil {
		return 0, err
	}
	return float64(n), nil
}

// parseDuration parses any positive time.ParseDuration string -- used
// directly for absent(sel, D)'s D, which has no stated minimum.
func (p *ruleParser) parseDuration() (time.Duration, error) {
	if p.tok.kind != tokWord {
		return 0, perr(p.tok.pos, "expected a duration")
	}
	text, pos := p.tok.text, p.tok.pos
	d, err := time.ParseDuration(text)
	if err != nil {
		return 0, perr(pos, "invalid duration %q", text)
	}
	if d <= 0 {
		return 0, perr(pos, "duration must be positive")
	}
	if err := p.advance(); err != nil {
		return 0, err
	}
	return d, nil
}

// parseForClause parses `for <dur>`, enforcing the task-7 ruling's 1m
// minimum on the `for` duration specifically.
func (p *ruleParser) parseForClause() (time.Duration, error) {
	if err := p.expectWord("for"); err != nil {
		return 0, err
	}
	pos := p.tok.pos
	d, err := p.parseDuration()
	if err != nil {
		return 0, err
	}
	if d < time.Minute {
		return 0, perr(pos, "for duration must be at least 1m")
	}
	return d, nil
}

// parseRuleExpr parses s per the task-7 grammar. It is also the Validate
// hook validateAlertingConfig calls (via validateRules) for every
// AggregateRule.Expr before it is ever saved, so a bad expression is
// rejected at `fleet alerting apply`/SetAlerting time, not silently ignored
// at evaluation time.
func parseRuleExpr(s string) (*ruleExpr, error) {
	p, err := newRuleParser(s)
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokWord {
		return nil, perr(p.tok.pos, "expected a rule function (count, avg, max, min, online, absent)")
	}
	fnTok := p.tok
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectKind(tokLParen, "'('"); err != nil {
		return nil, err
	}

	expr := &ruleExpr{Kind: fnTok.text}
	switch fnTok.text {
	case "count":
		if expr.Sel, err = p.parseSelector(); err != nil {
			return nil, err
		}
		if err := p.expectKind(tokComma, "','"); err != nil {
			return nil, err
		}
		if expr.Metric, err = p.parseMetric(); err != nil {
			return nil, err
		}
		if expr.InnerOp, err = p.parseOp(); err != nil {
			return nil, err
		}
		if expr.InnerNum, err = p.parseNumber(); err != nil {
			return nil, err
		}
		if err := p.expectKind(tokRParen, "')'"); err != nil {
			return nil, err
		}
		if expr.OuterOp, err = p.parseOp(); err != nil {
			return nil, err
		}
		if expr.OuterNum, err = p.parseInt(); err != nil {
			return nil, err
		}
		if expr.ForDur, err = p.parseForClause(); err != nil {
			return nil, err
		}

	case "avg", "max", "min":
		if expr.Sel, err = p.parseSelector(); err != nil {
			return nil, err
		}
		if err := p.expectKind(tokComma, "','"); err != nil {
			return nil, err
		}
		if expr.Metric, err = p.parseMetric(); err != nil {
			return nil, err
		}
		if err := p.expectKind(tokRParen, "')'"); err != nil {
			return nil, err
		}
		if expr.OuterOp, err = p.parseOp(); err != nil {
			return nil, err
		}
		if expr.OuterNum, err = p.parseNumber(); err != nil {
			return nil, err
		}
		if expr.ForDur, err = p.parseForClause(); err != nil {
			return nil, err
		}

	case "online":
		if expr.Sel, err = p.parseSelector(); err != nil {
			return nil, err
		}
		if err := p.expectKind(tokRParen, "')'"); err != nil {
			return nil, err
		}
		if expr.OuterOp, err = p.parseOp(); err != nil {
			return nil, err
		}
		if expr.OuterNum, err = p.parseInt(); err != nil {
			return nil, err
		}
		if expr.ForDur, err = p.parseForClause(); err != nil {
			return nil, err
		}

	case "absent":
		if expr.Sel, err = p.parseSelector(); err != nil {
			return nil, err
		}
		if err := p.expectKind(tokComma, "','"); err != nil {
			return nil, err
		}
		if expr.AbsentFor, err = p.parseDuration(); err != nil {
			return nil, err
		}
		if err := p.expectKind(tokRParen, "')'"); err != nil {
			return nil, err
		}

	default:
		return nil, perr(fnTok.pos, "unknown function %q", fnTok.text)
	}

	if p.tok.kind != tokEOF {
		return nil, perr(p.tok.pos, "unexpected trailing input")
	}
	return expr, nil
}

// ---- validation (Validate hook into validateAlertingConfig) --------------

// ruleNameRe is the task-7 ruling's exact rule-name grammar.
var ruleNameValid = func(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// validateRules is validateAlertingConfig's Validate hook for
// AlertingConfig.Rules (task 7): names must be unique and match
// [a-z0-9_-]{1,64}, every Expr must parse, and Severity (when set) must be
// "warning" or "critical" -- "" defaults to warning at evaluation time
// (ruleRuntimeSeverity), so it is accepted here too.
func validateRules(rules []core.AggregateRule) error {
	seen := map[string]bool{}
	for _, r := range rules {
		if !ruleNameValid(r.Name) {
			return fmt.Errorf("rule %q: invalid name (must match [a-z0-9_-]{1,64})", r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true
		if _, err := parseRuleExpr(r.Expr); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
		if r.Severity != "" && r.Severity != "warning" && r.Severity != "critical" {
			return fmt.Errorf("rule %q: severity must be \"warning\" or \"critical\"", r.Name)
		}
	}
	return nil
}

// ruleRuntimeSeverity is rule.Severity, defaulted to "warning" (task-7
// ruling: "the default is warning").
func ruleRuntimeSeverity(rule core.AggregateRule) string {
	if rule.Severity == "" {
		return "warning"
	}
	return rule.Severity
}

// ---- evaluation ------------------------------------------------------

// ruleState is one rule's in-memory runtime state (task-7 ruling: "keep a
// per-rule since timestamp in memory... after a restart it starts over").
type ruleState struct {
	since    int64 // unix time the condition first became continuously true; 0 = not currently true
	firing   bool
	value    float64
	hasValue bool
	noData   bool
	parseErr string
	severity string
}

// ruleEvalResult is one tick's raw evaluation of a single rule's condition,
// before the `for`-sustain state machine (fleetRuleEvaluator.evaluate) turns
// it into a fire/recover decision.
type ruleEvalResult struct {
	value    float64
	hasValue bool
	noData   bool // task-7 ruling: "no data" never fires/recovers, holds the previous firing state
	condTrue bool // meaningless when noData
}

// fleetRuleEvaluator holds the master-only data sources aggregate rules read
// (fleet-phase2-map.md section 8): the registry (tag/name matching,
// LastSeen), the liveness tracker (online/lagging/stale/down/revoked) and
// the replica sink (latest snapshot + 1m series), reached directly since
// this file lives in the same package. Wired once via
// fleetAlertEngine.SetRules; a nil evaluator (no SetRules call, every
// existing engine test) makes TickRules/RuleStates/ruleStillFiring all
// no-ops, exactly SetSilences's own nil-is-a-no-op pattern.
type fleetRuleEvaluator struct {
	reg     *fleet.Registry
	sink    *replicaSink
	tracker *fleet.Tracker
	started time.Time // absent(...)'s blind window measures from here

	mu       sync.Mutex
	states   map[string]*ruleState
	lastTick int64 // unix time of the last actual evaluation (ruleTickInterval gate)
}

func newFleetRuleEvaluator(reg *fleet.Registry, sink *replicaSink, tracker *fleet.Tracker, started time.Time) *fleetRuleEvaluator {
	return &fleetRuleEvaluator{reg: reg, sink: sink, tracker: tracker, started: started, states: map[string]*ruleState{}}
}

// due reports whether ruleTickInterval has elapsed since the last
// evaluation (self-gated exactly like TickSilences's lastSilencePush), and
// if so records now as the new last-tick time.
func (r *fleetRuleEvaluator) due(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastTick != 0 && now.Unix()-r.lastTick < int64(ruleTickInterval/time.Second) {
		return false
	}
	r.lastTick = now.Unix()
	return true
}

// ruleAction is one fire/recover TickRules's evaluation pass decided to
// submit, produced by evaluate while holding the lock and executed
// (e.Submit) after releasing it.
type ruleAction struct {
	name     string
	expr     string
	severity string
	kind     string // "fire" or "recover"
	value    float64
	hasValue bool
}

// evaluate re-evaluates every rule in rules against the current data
// sources, updates each rule's in-memory state, and returns every
// fire/recover transition that resulted -- a rule whose condition merely
// continues (or continues to not hold) produces nothing here; only an
// actual state change is ever submitted (task-7 ruling: alerts fire/recover
// on transition, not every tick, matching every other master-own alert in
// this codebase).
func (r *fleetRuleEvaluator) evaluate(now time.Time, rules []core.AggregateRule) []ruleAction {
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := map[string]bool{}
	var actions []ruleAction

	for _, rule := range rules {
		seen[rule.Name] = true
		st := r.states[rule.Name]
		if st == nil {
			st = &ruleState{}
			r.states[rule.Name] = st
		}
		st.severity = ruleRuntimeSeverity(rule)

		expr, err := parseRuleExpr(rule.Expr)
		if err != nil {
			// Shouldn't happen (validateRules already rejected a bad Expr at
			// Set time), but a saved config predating a stricter grammar, or
			// direct alerting.json surgery, must not panic or spam: treat it
			// as "no data" (never fires/recovers, holds whatever it was).
			st.parseErr = err.Error()
			st.noData = true
			continue
		}
		st.parseErr = ""

		res := r.evalExpr(expr, now)
		st.value, st.hasValue, st.noData = res.value, res.hasValue, res.noData
		if res.noData {
			continue // hold since/firing exactly as they were.
		}

		if res.condTrue {
			if st.since == 0 {
				st.since = now.Unix()
			}
			sustained := time.Duration(now.Unix()-st.since) * time.Second
			if !st.firing && sustained >= expr.forDuration() {
				st.firing = true
				actions = append(actions, ruleAction{name: rule.Name, expr: rule.Expr, severity: st.severity, kind: "fire", value: st.value, hasValue: st.hasValue})
			}
		} else {
			st.since = 0
			if st.firing {
				st.firing = false
				actions = append(actions, ruleAction{name: rule.Name, expr: rule.Expr, severity: st.severity, kind: "recover", value: st.value, hasValue: st.hasValue})
			}
		}
	}

	// A rule removed (or renamed) since the last evaluation while it was
	// firing recovers on this tick (task-7 ruling); everything about it is
	// then forgotten.
	for name, st := range r.states {
		if seen[name] {
			continue
		}
		if st.firing {
			actions = append(actions, ruleAction{name: name, severity: st.severity, kind: "recover", value: st.value, hasValue: st.hasValue})
		}
		delete(r.states, name)
	}

	return actions
}

// isFiring reports whether name is a currently-tracked rule that is firing.
// Used only by ruleStillFiring, which also checks the rule still exists in
// the live config -- see its doc comment.
func (r *fleetRuleEvaluator) isFiring(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.states[name]
	return ok && st.firing
}

// snapshot returns one core.RuleState per rule in rules (config order),
// merging in whatever runtime state has been observed for it so far (zero
// value -- never evaluated yet -- for a rule added since the last tick).
func (r *fleetRuleEvaluator) snapshot(rules []core.AggregateRule) []core.RuleState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]core.RuleState, 0, len(rules))
	for _, rule := range rules {
		rs := core.RuleState{Name: rule.Name, Expr: rule.Expr}
		if st := r.states[rule.Name]; st != nil {
			rs.Value, rs.HasValue, rs.Firing, rs.Since, rs.NoData, rs.Error = st.value, st.hasValue, st.firing, st.since, st.noData, st.parseErr
		}
		out = append(out, rs)
	}
	return out
}

// matchNodes returns every non-revoked registry node matching sel. A
// revoked node is excluded from every rule's selector match, consistently
// across count/online/avg/max/min/absent: it is effectively gone from the
// fleet (fleet_provider.go's Nodes/RevokeNode's own doc comments use the
// same framing), so an aggregate rule should neither count it toward
// "online" nor mistake its frozen LastSeen for a currently-absent report.
func (r *fleetRuleEvaluator) matchNodes(sel ruleSelector) []fleet.Node {
	var out []fleet.Node
	for _, n := range r.reg.List() {
		if n.Revoked {
			continue
		}
		if selectorMatches(sel, n) {
			out = append(out, n)
		}
	}
	return out
}

func selectorMatches(sel ruleSelector, n fleet.Node) bool {
	switch sel.Kind {
	case "all":
		return true
	case "tag":
		return slices.Contains(n.Tags, sel.Value)
	case "node":
		if sel.Value == n.ID {
			return true
		}
		ok, _ := path.Match(sel.Value, n.Name)
		return ok
	}
	return false
}

// nodeOnlineOrLagging reports whether id's tracked state counts as
// available for count()'s per-node eligibility gate and online()'s own
// tally (task-7 ruling: "A node is included only if its state is online or
// lagging").
func (r *fleetRuleEvaluator) nodeOnlineOrLagging(id string) bool {
	switch r.tracker.State(id) {
	case fleet.StateOnline, fleet.StateLagging:
		return true
	}
	return false
}

// snapshotOf returns id's latest parsed Snapshot from the replica sink's
// live view, exactly how fleet_provider.go's Nodes() reads it.
func (r *fleetRuleEvaluator) snapshotOf(id string) (Snapshot, bool) {
	u := r.sink.LiveOf(id)
	if u == nil {
		return Snapshot{}, false
	}
	var snap Snapshot
	if err := json.Unmarshal(u.Snapshot, &snap); err != nil {
		return Snapshot{}, false
	}
	return snap, true
}

// metricValue reads metric's current value off snap, per the task-7
// metric-to-snapshot-field mapping ("disk" is the worst mount, via the same
// worstDisk helper fleet_provider.go's Nodes() uses).
func metricValue(metric string, snap Snapshot) float64 {
	switch metric {
	case "cpu":
		return snap.CPU
	case "mem":
		return snap.MemPct
	case "swap":
		return snap.SwapPct
	case "disk":
		return worstDisk(snap.Disks)
	case "load1":
		return snap.Load1
	case "temp":
		return snap.TempC
	}
	return 0
}

// compare applies op (> >= < <= == !=) to v against target.
func compare(v float64, op string, target float64) bool {
	switch op {
	case ">":
		return v > target
	case ">=":
		return v >= target
	case "<":
		return v < target
	case "<=":
		return v <= target
	case "==":
		return v == target
	case "!=":
		return v != target
	}
	return false
}

func (r *fleetRuleEvaluator) evalExpr(expr *ruleExpr, now time.Time) ruleEvalResult {
	switch expr.Kind {
	case "count":
		return r.evalCount(expr)
	case "online":
		return r.evalOnline(expr)
	case "avg", "max", "min":
		return r.evalAggSeries(expr, now)
	case "absent":
		return r.evalAbsent(expr, now)
	}
	return ruleEvalResult{}
}

// evalCount implements count(<sel>, <metric> <op> <num>) <op> <int>: a
// per-node condition (metric compared against InnerNum) counted across
// every selector-matching node currently online or lagging (task-7 ruling:
// "For count, stale, down and revoked nodes are skipped"), then that COUNT
// compared against OuterNum.
func (r *fleetRuleEvaluator) evalCount(expr *ruleExpr) ruleEvalResult {
	count := 0
	for _, n := range r.matchNodes(expr.Sel) {
		if !r.nodeOnlineOrLagging(n.ID) {
			continue
		}
		snap, ok := r.snapshotOf(n.ID)
		if !ok {
			continue
		}
		if compare(metricValue(expr.Metric, snap), expr.InnerOp, expr.InnerNum) {
			count++
		}
	}
	return ruleEvalResult{value: float64(count), hasValue: true, condTrue: compare(float64(count), expr.OuterOp, expr.OuterNum)}
}

// evalOnline implements online(<sel>) <op> <int>: the count of
// selector-matching nodes currently online or lagging (task-7 ruling: "For
// online, they [stale/down/revoked] count as not online").
func (r *fleetRuleEvaluator) evalOnline(expr *ruleExpr) ruleEvalResult {
	online := 0
	for _, n := range r.matchNodes(expr.Sel) {
		if r.nodeOnlineOrLagging(n.ID) {
			online++
		}
	}
	return ruleEvalResult{value: float64(online), hasValue: true, condTrue: compare(float64(online), expr.OuterOp, expr.OuterNum)}
}

// nodeSeriesAverage returns id's average value of metric over [from, to] at
// 1m resolution, and whether it has any points at all in that window (task-7
// ruling: "If a node has no points in the window, exclude it"). "disk" is
// special-cased to the worst mount's own average (nodeDiskSeriesAverage).
func (r *fleetRuleEvaluator) nodeSeriesAverage(id, metric string, from, to int64) (float64, bool) {
	n, err := r.sink.node(id)
	if err != nil {
		return 0, false
	}
	if metric == "disk" {
		return nodeDiskSeriesAverage(n, from, to)
	}
	pts, err := n.store.Query(metric, from, to, Res1m)
	if err != nil || len(pts) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, p := range pts {
		sum += p.Avg
	}
	return sum / float64(len(pts)), true
}

// nodeDiskSeriesAverage computes, for every "disk:<mount>" 1m series n has,
// that mount's own average over [from, to], and returns the WORST (highest)
// of those per-mount averages -- "disk (worst)", the same framing
// worstDisk(snap.Disks) uses for the instantaneous case (evalCount), applied
// here across the window instead of a single point in time.
func nodeDiskSeriesAverage(n *replicaNode, from, to int64) (float64, bool) {
	metrics, err := n.store.Metrics(Res1m)
	if err != nil {
		return 0, false
	}
	worst, has := 0.0, false
	for _, m := range metrics {
		if !strings.HasPrefix(m, "disk:") {
			continue
		}
		pts, err := n.store.Query(m, from, to, Res1m)
		if err != nil || len(pts) == 0 {
			continue
		}
		sum := 0.0
		for _, p := range pts {
			sum += p.Avg
		}
		avg := sum / float64(len(pts))
		if !has || avg > worst {
			worst, has = avg, true
		}
	}
	return worst, has
}

// evalAggSeries implements avg|max|min(<sel>, <metric>) <op> <num>: each
// selector-matching node's own AVERAGE of metric over the `for` window (a
// node with no points in the window is excluded entirely -- task-7 ruling),
// then the rule's own aggregation function (avg/max/min) applied across
// those per-node averages. If NO node has any data, the result is "no data"
// (task-7 ruling: "does not fire and does not recover; it holds the
// previous state").
func (r *fleetRuleEvaluator) evalAggSeries(expr *ruleExpr, now time.Time) ruleEvalResult {
	from := now.Add(-expr.ForDur).Unix()
	to := now.Unix()
	var perNode []float64
	for _, n := range r.matchNodes(expr.Sel) {
		if v, ok := r.nodeSeriesAverage(n.ID, expr.Metric, from, to); ok {
			perNode = append(perNode, v)
		}
	}
	if len(perNode) == 0 {
		return ruleEvalResult{noData: true}
	}
	agg := perNode[0]
	switch expr.Kind {
	case "avg":
		sum := 0.0
		for _, v := range perNode {
			sum += v
		}
		agg = sum / float64(len(perNode))
	case "max":
		for _, v := range perNode[1:] {
			if v > agg {
				agg = v
			}
		}
	case "min":
		for _, v := range perNode[1:] {
			if v < agg {
				agg = v
			}
		}
	}
	return ruleEvalResult{value: agg, hasValue: true, condTrue: compare(agg, expr.OuterOp, expr.OuterNum)}
}

// evalAbsent implements absent(<sel>, <dur>): fires when no selector-matching
// node (including the case where none match at all) has a LastSeen within
// AbsentFor of now, but only once the master itself has been up at least
// AbsentFor (task-7 ruling's blind window) -- during that window condTrue is
// forced false, not "no data" (the state is known: deliberately not yet
// judged). value is the number of seconds since the most recently seen
// matching node last reported (or since the master started, if none match
// at all), which is not specified by the brief but is the one number that
// makes the alert title/CLI value column meaningful for this rule kind.
func (r *fleetRuleEvaluator) evalAbsent(expr *ruleExpr, now time.Time) ruleEvalResult {
	maxLastSeen := int64(-1)
	for _, n := range r.matchNodes(expr.Sel) {
		if n.LastSeen > maxLastSeen {
			maxLastSeen = n.LastSeen
		}
	}
	var value float64
	if maxLastSeen >= 0 {
		value = float64(now.Unix() - maxLastSeen)
	} else {
		value = now.Sub(r.started).Seconds()
	}
	cutoff := now.Add(-expr.AbsentFor).Unix()
	reported := maxLastSeen >= cutoff
	blind := now.Sub(r.started) < expr.AbsentFor
	return ruleEvalResult{value: value, hasValue: true, condTrue: !reported && !blind}
}

// formatRuleValue renders a rule's current value for its alert title/CLI
// display: the shortest exact decimal representation, or "n/a" if the rule
// has never produced a value (shouldn't happen for a fire/recover, which
// only ever follow a real evaluation, but kept defensive).
func formatRuleValue(v float64, hasValue bool) string {
	if !hasValue {
		return "n/a"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ---- fleetAlertEngine wiring -------------------------------------------

// SetRules wires the aggregate-rule evaluator (task 7), mirroring
// SetSilences/SetRouting/SetDependencies's own "called once from
// startMaster, after both the engine and its data sources exist" pattern. A
// nil e.rules (no SetRules call, every existing engine test) makes
// TickRules/RuleStates/ruleStillFiring all no-ops.
func (e *fleetAlertEngine) SetRules(reg *fleet.Registry, sink *replicaSink, tracker *fleet.Tracker, started time.Time) {
	e.rules = newFleetRuleEvaluator(reg, sink, tracker, started)
}

// TickRules evaluates every configured aggregate rule at most once every
// ruleTickInterval (task 7: "every 30s"), submitting a fire/recover through
// Submit -- exactly like any other master-own alert -- for every rule whose
// firing state just changed.
func (e *fleetAlertEngine) TickRules(now time.Time) {
	if e.rules == nil || e.alerting == nil {
		return
	}
	if !e.rules.due(now) {
		return
	}
	cfg := e.alerting.Get()
	for _, act := range e.rules.evaluate(now, cfg.Rules) {
		e.submitRuleAlert(act, now)
	}
}

// ruleAlertKey is the one place "fleet:rule:<name>"'s exact format is built
// (task-7 ruling), matched by masterLoop.stillActive's own case.
func ruleAlertKey(name string) string { return "fleet:rule:" + name }

// submitRuleAlert builds and submits one rule's fire/recover alert (task-7
// ruling: key "fleet:rule:<name>", title "rule <name>: <expr> (value <v>)",
// severity from the rule).
func (e *fleetAlertEngine) submitRuleAlert(act ruleAction, now time.Time) {
	sev, err := ParseSeverity(act.severity)
	if err != nil {
		sev = SevWarning
	}
	title := fmt.Sprintf("rule %s: %s (value %s)", act.name, act.expr, formatRuleValue(act.value, act.hasValue))
	e.Submit(alertSource{}, Alert{
		Key: ruleAlertKey(act.name), Title: title, Severity: sev, Kind: act.kind, Time: now.Unix(),
	})
}

// RuleStates implements core.FleetAPI.RuleStates: the current value/firing
// state of every configured rule, in config order.
func (e *fleetAlertEngine) RuleStates() []core.RuleState {
	if e.rules == nil || e.alerting == nil {
		return nil
	}
	return e.rules.snapshot(e.alerting.Get().Rules)
}

// ruleStillFiring implements the task-7 stillActive extension: false if the
// rule no longer exists in the live config, or its current in-memory state
// is not firing -- so an orphaned "fleet:rule:<name>" incident (open across
// a master restart that then removed or resolved the rule) recovers via
// checkOrphanedIncidents instead of paging forever.
func (e *fleetAlertEngine) ruleStillFiring(name string) bool {
	if e.rules == nil || e.alerting == nil {
		return false
	}
	exists := false
	for _, r := range e.alerting.Get().Rules {
		if r.Name == name {
			exists = true
			break
		}
	}
	if !exists {
		return false
	}
	return e.rules.isFiring(name)
}
