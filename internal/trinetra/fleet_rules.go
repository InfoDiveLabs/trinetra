// Package trinetra: fleet_rules.go is the master's aggregate-rule engine a small,
// hand-written grammar (NOT PromQL) parsed by parseRuleExpr.
package trinetra

import (
	"encoding/json"
	"fmt"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// ruleTickInterval is how often TickRules actually evaluates rules (30s), self-gated inside
// TickRules like TickSilences's silencePushInterval.
const ruleTickInterval = 30 * time.Second

// ---- grammar: tokenizer -----------------------------------------------

// ruleParseError is parseRuleExpr's error type: every error names the position (byte offset
// into the original expression) it was found at, e.g. "expected ',' at 12".
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

// ruleLexer is a small hand-written scanner: every token is punctuation ('(', ')', ','), a
// comparison operator (> >= < <= == !=), or a "word", a run of characters a selector.
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
	// c is only the first BYTE, so decode a full UTF-8 rune (string(byte) would read it as a
	// Latin-1 code point and garble non-ASCII).
	r, size := utf8.DecodeRuneInString(lx.s[lx.pos:])
	if r == utf8.RuneError && size <= 1 {
		lx.pos++
		return ruleToken{}, perr(start, "unexpected character %q", lx.s[start:start+1])
	}
	lx.pos += size
	return ruleToken{}, perr(start, "unexpected character %q", string(r))
}

// ---- grammar: AST --------------------------------------------------------

// ruleSelector is <sel>: tag:<t>, node:<glob> (an exact node id or a path.Match glob
// against the node's display name, exactly Matcher.Node's own semantics) or all.
type ruleSelector struct {
	Kind  string // "tag", "node", "all"
	Value string // the tag name, or the node id-or-glob; "" for "all"
}

// ruleExpr is one parsed aggregate-rule expression.
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

// forDuration is the sustain window a rule's condition must hold continuously before it
// fires: ForDur for every kind but absent, which has none.
func (e *ruleExpr) forDuration() time.Duration {
	if e.Kind == "absent" {
		return 0
	}
	return e.ForDur
}

// ruleMetrics are the only metric names a count/avg/max/min rule may name
// disk is the WORST mount, not a specific one.
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
	// ParseFloat accepts "NaN"/"Inf"/"Infinity" (any case, signed), none of which is a sane
	// threshold: every compare() against one is constant or undefined.
	if math.IsNaN(v) || math.IsInf(v, 0) {
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

// parseForClause parses `for <dur>`, enforcing a 1m minimum on the duration.
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

// parseRuleExpr parses s per the rule grammar.
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

// ruleNameRe is the rule-name grammar.
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

// validateRules is validateAlertingConfig's Validate hook for AlertingConfig.Rules: names
// must be unique and match [a-z0-9_-]{1,64}, every Expr must parse, and Severity.
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

// ruleRuntimeSeverity is rule.Severity, defaulting to "warning".
func ruleRuntimeSeverity(rule core.AggregateRule) string {
	if rule.Severity == "" {
		return "warning"
	}
	return rule.Severity
}

// ---- evaluation ------------------------------------------------------

// ruleState is one rule's in-memory runtime state (a per-rule since timestamp;
// after a restart it starts over).
type ruleState struct {
	since    int64 // unix time the condition first became continuously true; 0 = not currently true
	firing   bool
	value    float64
	hasValue bool
	noData   bool
	parseErr string
	severity string // the last-APPLIED effective severity (detects an in-place edit)
	expr     string // the last-APPLIED Expr text (detects an in-place edit)
	// initialized is false only before this rule's very first evaluation, so a brand new
	// rule's expr/severity (compared against the zero value "") is not mistaken for an "edit".
	initialized bool
}

// ruleEvalResult is one tick's raw evaluation of a single rule's condition, before the
// `for`-sustain state machine.
type ruleEvalResult struct {
	value    float64
	hasValue bool
	noData   bool // "no data" never fires/recovers, holds the previous firing state
	condTrue bool // meaningless when noData
}

// ruleSelfSource bundles the master's own node data for aggregate rules `all` and
// `node:<glob>` selectors must include the master's own node.
type ruleSelfSource struct {
	// Name is the master's own display name (fleetProvider.selfName, ultimately
	// config.ServerName()) -- what a `node:<glob>` selector globs self's name against.
	Name func() string
	// Snap returns the master's own current Snapshot and whether one has ever been collected
	// yet.
	Snap func() (Snapshot, bool)
	// Store is the master's own local SampleStore (fleetDeps.store): the same series
	// count()/avg()/max()/min() read for every other node, just local instead of replicated.
	Store SampleStore
}

// fleetRuleEvaluator holds the master-only data sources aggregate rules read
// (fleet-phase2-map.md section 8): the registry (tag/name matching, LastSeen).
type fleetRuleEvaluator struct {
	reg     *fleet.Registry
	sink    *replicaSink
	tracker *fleet.Tracker
	started time.Time // absent(...)'s blind window measures from here
	self    ruleSelfSource

	mu       sync.Mutex
	states   map[string]*ruleState
	lastTick int64 // unix time of the last actual evaluation (ruleTickInterval gate)
}

func newFleetRuleEvaluator(reg *fleet.Registry, sink *replicaSink, tracker *fleet.Tracker, started time.Time, self ruleSelfSource) *fleetRuleEvaluator {
	return &fleetRuleEvaluator{reg: reg, sink: sink, tracker: tracker, started: started, self: self, states: map[string]*ruleState{}}
}

// due reports whether ruleTickInterval has elapsed since the last evaluation (self-gated
// exactly like TickSilences's lastSilencePush).
func (r *fleetRuleEvaluator) due(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastTick != 0 && now.Unix()-r.lastTick < int64(ruleTickInterval/time.Second) {
		return false
	}
	r.lastTick = now.Unix()
	return true
}

// ruleAction is one fire/recover TickRules's evaluation pass decided to submit, produced by
// evaluate while holding the lock and executed (e.Submit) after releasing it.
type ruleAction struct {
	name     string
	expr     string
	severity string
	kind     string // "fire" or "recover"
	value    float64
	hasValue bool
}

// evaluate re-evaluates every rule in rules against the current data sources, updates each
// rule's in-memory state, and returns every fire/recover transition.
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

		// An in-place edit of a firing rule (same Name, different Expr or Severity) must not be a
		// silent no-op.
		effSeverity := ruleRuntimeSeverity(rule)
		oldExpr, oldSeverity := st.expr, st.severity
		if edited := st.initialized && (rule.Expr != oldExpr || effSeverity != oldSeverity); edited {
			if st.firing {
				actions = append(actions, ruleAction{name: rule.Name, expr: oldExpr, severity: oldSeverity, kind: "recover", value: st.value, hasValue: st.hasValue})
			}
			// Reset to a completely fresh state: the re-evaluation below must re-establish `since`
			// from scratch and so cannot fire on this same tick.
			st.since, st.firing, st.value, st.hasValue, st.noData = 0, false, 0, false, false
		}
		st.expr, st.severity, st.initialized = rule.Expr, effSeverity, true

		expr, err := parseRuleExpr(rule.Expr)
		if err != nil {
			// Shouldn't happen (validateRules already rejected a bad Expr at Set time), but a saved
			// config predating a stricter grammar, or direct alerting.json surgery.
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

	// A rule removed (or renamed) since the last evaluation while it was firing recovers on
	// this tick; everything about it is then forgotten.
	for name, st := range r.states {
		if seen[name] {
			continue
		}
		if st.firing {
			actions = append(actions, ruleAction{name: name, expr: st.expr, severity: st.severity, kind: "recover", value: st.value, hasValue: st.hasValue})
		}
		delete(r.states, name)
	}

	return actions
}

// isFiring reports whether name is a currently-tracked rule that is firing.
func (r *fleetRuleEvaluator) isFiring(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.states[name]
	return ok && st.firing
}

// snapshot returns one core.RuleState per rule in rules (config order), merging in whatever
// runtime state has been observed for it so far.
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

// matchNodes returns whether sel matches the master's own "self" node and every non-revoked
// REGISTRY node matching sel.
func (r *fleetRuleEvaluator) matchNodes(sel ruleSelector) (includeSelf bool, nodes []fleet.Node) {
	includeSelf = r.selfMatches(sel)
	for _, n := range r.reg.List() {
		if n.Revoked {
			continue
		}
		if selectorMatches(sel, n) {
			nodes = append(nodes, n)
		}
	}
	return includeSelf, nodes
}

// selfMatches reports whether sel matches the master's own node: `all` always includes
// self; `node:<glob>` includes self when the glob matches its display name.
func (r *fleetRuleEvaluator) selfMatches(sel ruleSelector) bool {
	switch sel.Kind {
	case "all":
		return true
	case "node":
		if sel.Value == core.SelfNodeID {
			return true
		}
		name := r.selfNodeName()
		if name == "" {
			return false
		}
		ok, _ := path.Match(sel.Value, name)
		return ok
	}
	return false
}

func (r *fleetRuleEvaluator) selfNodeName() string {
	if r.self.Name == nil {
		return ""
	}
	return r.self.Name()
}

// selfSnapshot returns the master's own current Snapshot, or false if
// ruleSelfSource.Snap is unset or reports none collected yet.
func (r *fleetRuleEvaluator) selfSnapshot() (Snapshot, bool) {
	if r.self.Snap == nil {
		return Snapshot{}, false
	}
	return r.self.Snap()
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

// nodeOnlineOrLagging reports whether id's tracked state counts as available for count()'s
// per-node eligibility gate and online()'s tally (only online or lagging).
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

// metricValue reads metric's current value off snap ("disk" is the worst
// mount, via the same worstDisk helper fleet_provider.go's Nodes() uses).
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

// evalCount implements count(<sel>, <metric> <op> <num>) <op> <int>: a per-node condition.
func (r *fleetRuleEvaluator) evalCount(expr *ruleExpr) ruleEvalResult {
	count := 0
	includeSelf, nodes := r.matchNodes(expr.Sel)
	if includeSelf {
		if snap, ok := r.selfSnapshot(); ok && compare(metricValue(expr.Metric, snap), expr.InnerOp, expr.InnerNum) {
			count++
		}
	}
	for _, n := range nodes {
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

// evalOnline implements online(<sel>) <op> <int>: the count of selector-matching nodes
// currently online or lagging (stale/down/revoked count as not online).
func (r *fleetRuleEvaluator) evalOnline(expr *ruleExpr) ruleEvalResult {
	online := 0
	includeSelf, nodes := r.matchNodes(expr.Sel)
	if includeSelf {
		online++
	}
	for _, n := range nodes {
		if r.nodeOnlineOrLagging(n.ID) {
			online++
		}
	}
	return ruleEvalResult{value: float64(online), hasValue: true, condTrue: compare(float64(online), expr.OuterOp, expr.OuterNum)}
}

// nodeSeriesAverage returns id's average value of metric over [from, to] at 1m resolution,
// and whether it has any points in that window (a node with none is excluded).
func (r *fleetRuleEvaluator) nodeSeriesAverage(id, metric string, from, to int64) (float64, bool) {
	n, err := r.sink.node(id)
	if err != nil {
		return 0, false
	}
	return seriesAverage(n.store, metric, from, to)
}

// seriesAverage returns store's average value of metric over [from, to] at 1m resolution,
// and whether it has any points in that window.
func seriesAverage(store SampleStore, metric string, from, to int64) (float64, bool) {
	if metric == "disk" {
		return diskSeriesAverage(store, from, to)
	}
	pts, err := store.Query(metric, from, to, Res1m)
	if err != nil || len(pts) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, p := range pts {
		sum += p.Avg
	}
	return sum / float64(len(pts)), true
}

// seriesMetricLister is the subset of SampleStore's concrete backends (today only
// *tsFileStore) that can enumerate which metrics they hold.
type seriesMetricLister interface {
	Metrics(res Resolution) ([]string, error)
}

// diskMountMetrics returns every "disk:<mount>" series name store holds at res, or ok=false
// when store's backend can't enumerate its series at all (see seriesMetricLister).
func diskMountMetrics(store SampleStore, res Resolution) ([]string, bool) {
	lister, ok := store.(seriesMetricLister)
	if !ok {
		return nil, false
	}
	all, err := lister.Metrics(res)
	if err != nil {
		return nil, false
	}
	var out []string
	for _, m := range all {
		if strings.HasPrefix(m, "disk:") {
			out = append(out, m)
		}
	}
	return out, true
}

// diskSeriesAverage computes, for every "disk:<mount>" 1m series store has, that mount's
// own average over [from, to], and returns the WORST (highest) of those per-mount averages.
func diskSeriesAverage(store SampleStore, from, to int64) (float64, bool) {
	metrics, ok := diskMountMetrics(store, Res1m)
	if !ok {
		return 0, false
	}
	worst, has := 0.0, false
	for _, m := range metrics {
		pts, err := store.Query(m, from, to, Res1m)
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

// evalAggSeries implements avg|max|min(<sel>, <metric>) <op> <num>: each selector-matching
// node's own AVERAGE of metric over the `for` window.
func (r *fleetRuleEvaluator) evalAggSeries(expr *ruleExpr, now time.Time) ruleEvalResult {
	from := now.Add(-expr.ForDur).Unix()
	to := now.Unix()
	var perNode []float64
	includeSelf, nodes := r.matchNodes(expr.Sel)
	if includeSelf && r.self.Store != nil {
		if v, ok := seriesAverage(r.self.Store, expr.Metric, from, to); ok {
			perNode = append(perNode, v)
		}
	}
	for _, n := range nodes {
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

// evalAbsent implements absent(<sel>, <dur>): fires when no selector-matching node
// (including none matching at all) has a LastSeen within AbsentFor of now.
func (r *fleetRuleEvaluator) evalAbsent(expr *ruleExpr, now time.Time) ruleEvalResult {
	includeSelf, nodes := r.matchNodes(expr.Sel)
	maxLastSeen := int64(0) // 0 means "no matching node has ever reported"
	if includeSelf {
		maxLastSeen = now.Unix()
	}
	for _, n := range nodes {
		if n.LastSeen > maxLastSeen {
			maxLastSeen = n.LastSeen
		}
	}
	var value float64
	if maxLastSeen > 0 {
		value = float64(now.Unix() - maxLastSeen)
	} else {
		value = now.Sub(r.started).Seconds()
	}
	cutoff := now.Add(-expr.AbsentFor).Unix()
	reported := maxLastSeen > 0 && maxLastSeen >= cutoff
	blind := now.Sub(r.started) < expr.AbsentFor
	return ruleEvalResult{value: value, hasValue: true, condTrue: !reported && !blind}
}

// formatRuleValue renders a rule's current value for its alert title/CLI display: the
// shortest exact decimal representation, or "n/a" if the rule has never produced a value.
func formatRuleValue(v float64, hasValue bool) string {
	if !hasValue {
		return "n/a"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ---- fleetAlertEngine wiring -------------------------------------------

// SetRules wires the aggregate-rule evaluator, called once from startMaster after both the
// engine and its data sources exist.
func (e *fleetAlertEngine) SetRules(reg *fleet.Registry, sink *replicaSink, tracker *fleet.Tracker, started time.Time, self ruleSelfSource) {
	e.rules = newFleetRuleEvaluator(reg, sink, tracker, started, self)
}

// TickRules evaluates every configured aggregate rule at most once every ruleTickInterval,
// submitting a fire/recover through Submit, like any other master-own alert.
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
// matched by masterLoop.stillActive's own case.
func ruleAlertKey(name string) string { return "fleet:rule:" + name }

// submitRuleAlert builds and submits one rule's fire/recover alert: key
// "fleet:rule:<name>", title "rule <name>: <expr> (value <v>)", severity from the rule.
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

// ruleStillFiring implements stillActive for rules: false if the rule no longer exists in
// the live config or its in-memory state is not firing.
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
