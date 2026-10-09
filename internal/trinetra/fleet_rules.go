// Package trinetra: fleet_rules.go is the master's aggregate-rule engine
// a small, hand-written grammar (NOT PromQL) parsed by
// parseRuleExpr, evaluated every ruleTickInterval by fleetAlertEngine.TickRules
// against the same data sources fleet_provider.go's Nodes() and the child's
// own gap-filler already use -- the replica sink's latest snapshot
// (replicaSink.LiveOf) and 1m series (replicaNode.store.Query), the liveness
// tracker's state, and the registry's tags/LastSeen (see
// fleet-phase2-map.md section 8).
//
// Grammar:
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

// ruleTickInterval is how often TickRules actually evaluates rules (30s),
// self-gated inside TickRules like TickSilences's silencePushInterval:
// masterLoop.tick calls it every 5s and the gate decides whether to act.
const ruleTickInterval = 30 * time.Second

// ---- grammar: tokenizer -----------------------------------------------

// ruleParseError is parseRuleExpr's error type: every error names the position
// (byte offset into the original expression) it was found at, e.g.
// "expected ',' at 12".
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

// ruleLexer is a small hand-written scanner: every token is punctuation
// ('(', ')', ','), a comparison operator (> >= < <= == !=), or a "word", a run
// of characters a selector, metric name, number, duration or the `for` keyword
// can be made of (letters, digits, ':', '_', '-', '.', '*', '?', '[', ']',
// '/'). Operator characters ('>', '<', '=', '!') are excluded so a word never
// swallows the comparison that follows it.
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
	// c is only the first BYTE, so decode a full UTF-8 rune (string(byte)
	// would read it as a Latin-1 code point and garble non-ASCII). An invalid
	// encoding still advances one byte so the lexer can't loop forever.
	r, size := utf8.DecodeRuneInString(lx.s[lx.pos:])
	if r == utf8.RuneError && size <= 1 {
		lx.pos++
		return ruleToken{}, perr(start, "unexpected character %q", lx.s[start:start+1])
	}
	lx.pos += size
	return ruleToken{}, perr(start, "unexpected character %q", string(r))
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
	// ParseFloat accepts "NaN"/"Inf"/"Infinity" (any case, signed), none of
	// which is a sane threshold: every compare() against one is constant or
	// undefined. Reject them like an unparseable number.
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

// parseRuleExpr parses s per the rule grammar. It is also the Validate hook
// validateAlertingConfig calls (via validateRules) for every
// AggregateRule.Expr before it is saved, so a bad expression is rejected at
// `fleet alerting apply`/SetAlerting time, not ignored at evaluation time.
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

// validateRules is validateAlertingConfig's Validate hook for
// AlertingConfig.Rules: names must be unique and match
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
	// initialized is false only before this rule's very first evaluation, so a
	// brand new rule's expr/severity (compared against the zero value "") is not
	// mistaken for an "edit": a genuine edit needs a PRIOR applied definition.
	initialized bool
}

// ruleEvalResult is one tick's raw evaluation of a single rule's condition,
// before the `for`-sustain state machine (fleetRuleEvaluator.evaluate) turns
// it into a fire/recover decision.
type ruleEvalResult struct {
	value    float64
	hasValue bool
	noData   bool // "no data" never fires/recovers, holds the previous firing state
	condTrue bool // meaningless when noData
}

// ruleSelfSource bundles the master's own node data for aggregate rules
// `all` and `node:<glob>` selectors must
// include the master's own node ("self" never appears in the registry, so
// without this it was silently excluded from every aggregate rule). Every
// field is nil-safe (a fleetRuleEvaluator built with the zero value, e.g. by
// an older test, simply never matches self) -- Name/Snap/Store mirror
// exactly what `fleetProvider`/`fleetDeps` already expose in production
// (selfName, latestSnapshot, and the daemon's own local SampleStore).
type ruleSelfSource struct {
	// Name is the master's own display name (fleetProvider.selfName,
	// ultimately config.ServerName()) -- what a `node:<glob>` selector globs
	// self's name against.
	Name func() string
	// Snap returns the master's own current Snapshot and whether one has
	// ever been collected yet (false before the sampler loop's first tick --
	// see snapshot_hub.go's latestSnapshot doc comment).
	Snap func() (Snapshot, bool)
	// Store is the master's own local SampleStore (fleetDeps.store): the
	// same series count()/avg()/max()/min() read for every other node, just
	// local instead of replicated. Its "disk (worst)" handling needs to list
	// this store's own "disk:<mount>" series; not every SampleStore backend
	// can (see diskSeriesAverage), so that case degrades to "no data" for
	// self rather than failing the whole rule.
	Store SampleStore
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
	self    ruleSelfSource

	mu       sync.Mutex
	states   map[string]*ruleState
	lastTick int64 // unix time of the last actual evaluation (ruleTickInterval gate)
}

func newFleetRuleEvaluator(reg *fleet.Registry, sink *replicaSink, tracker *fleet.Tracker, started time.Time, self ruleSelfSource) *fleetRuleEvaluator {
	return &fleetRuleEvaluator{reg: reg, sink: sink, tracker: tracker, started: started, self: self, states: map[string]*ruleState{}}
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

// evaluate re-evaluates every rule in rules against the current data sources,
// updates each rule's in-memory state, and returns every fire/recover
// transition. A rule whose condition merely continues produces nothing: alerts
// fire/recover on transition, not every tick, like every other master-own
// alert.
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

		// An in-place edit of a firing rule (same Name, different Expr or Severity)
		// must not be a silent no-op: otherwise the old firing/since state would keep
		// being evaluated against the NEW Expr, with no recover for the old
		// definition and no fresh `for` sustain window. Detect it here (only once the
		// rule has been evaluated before; see ruleState.initialized).
		effSeverity := ruleRuntimeSeverity(rule)
		oldExpr, oldSeverity := st.expr, st.severity
		if edited := st.initialized && (rule.Expr != oldExpr || effSeverity != oldSeverity); edited {
			if st.firing {
				actions = append(actions, ruleAction{name: rule.Name, expr: oldExpr, severity: oldSeverity, kind: "recover", value: st.value, hasValue: st.hasValue})
			}
			// Reset to a completely fresh state: the re-evaluation below must
			// re-establish `since` from scratch and so cannot fire on this
			// same tick (its own `for` sustain has not elapsed yet), even
			// though the underlying condition may already read true under the
			// new definition.
			st.since, st.firing, st.value, st.hasValue, st.noData = 0, false, 0, false, false
		}
		st.expr, st.severity, st.initialized = rule.Expr, effSeverity, true

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
	// firing recovers on this tick; everything about it is
	// then forgotten.
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

// matchNodes returns whether sel matches the master's own "self" node
// and every non-revoked
// REGISTRY node matching sel. A revoked node is excluded from every rule's
// selector match, consistently across count/online/avg/max/min/absent: it
// is effectively gone from the fleet (fleet_provider.go's Nodes/RevokeNode's
// own doc comments use the same framing), so an aggregate rule should
// neither count it toward "online" nor mistake its frozen LastSeen for a
// currently-absent report.
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

// selfMatches reports whether sel matches the master's own node: `all` always
// includes self; `node:<glob>` includes self when the glob matches its display
// name (ServerName(), via ruleSelfSource.Name) OR names the exact `self` id
// (core.SelfNodeID), the same "id-exact-or-name-glob" shape core.Matcher.Node
// uses. `tag:<t>` never matches self: the master's own node carries no tags.
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

// nodeOnlineOrLagging reports whether id's tracked state counts as available
// for count()'s per-node eligibility gate and online()'s tally (only online or
// lagging).
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

// evalCount implements count(<sel>, <metric> <op> <num>) <op> <int>: a
// per-node condition (metric compared against InnerNum) counted across every
// selector-matching node currently online or lagging (stale, down and revoked
// nodes are skipped), then that COUNT compared against OuterNum. Self is
// always eligible (its state is definitionally online), so it contributes
// whenever it matches sel and a current snapshot is available.
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

// evalOnline implements online(<sel>) <op> <int>: the count of
// selector-matching nodes currently online or lagging (stale/down/revoked count
// as not online). Self always counts as online when it matches sel, even if no
// snapshot has ever been collected for it.
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

// nodeSeriesAverage returns id's average value of metric over [from, to] at 1m
// resolution, and whether it has any points in that window (a node with none
// is excluded).
func (r *fleetRuleEvaluator) nodeSeriesAverage(id, metric string, from, to int64) (float64, bool) {
	n, err := r.sink.node(id)
	if err != nil {
		return 0, false
	}
	return seriesAverage(n.store, metric, from, to)
}

// seriesAverage returns store's average value of metric over [from, to] at 1m
// resolution, and whether it has any points in that window. "disk" is
// special-cased to the worst mount's own average (diskSeriesAverage). Shared by
// nodeSeriesAverage (a replicated node's *tsFileStore) and the master's own
// local store (ruleSelfSource.Store); both satisfy SampleStore.
//
// TODO(perf): every rule with a series-based condition (avg/max/min) queries
// its own window fresh, per node, every TickRules pass; two rules reading
// the same (node, metric, window) today issue the query twice. If aggregate
// rules grow numerous enough for this to matter, cache Query results keyed
// by (node/self, metric, from, to) for the duration of one evaluate() call.
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

// seriesMetricLister is the subset of SampleStore's concrete backends (today
// only *tsFileStore) that can enumerate which metrics they hold -- needed to
// discover a node's "disk:<mount>" series without knowing the mount names in
// advance. Not every SampleStore backend (e.g. the in-memory "memory"
// storage.backend option) implements it; diskSeriesAverage degrades to "no
// data" rather than failing the whole rule when it doesn't.
type seriesMetricLister interface {
	Metrics(res Resolution) ([]string, error)
}

// diskMountMetrics returns every "disk:<mount>" series name store holds at
// res, or ok=false when store's backend can't enumerate its series at all
// (see seriesMetricLister) -- shared by diskSeriesAverage (single-value,
// used by the aggregate-rule engine) and fleet_provider.go's diskSeriesPoints
// so both read the exact
// same "which mounts does this node have" answer instead of duplicating the
// enumeration.
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

// diskSeriesAverage computes, for every "disk:<mount>" 1m series store has,
// that mount's own average over [from, to], and returns the WORST (highest)
// of those per-mount averages -- "disk (worst)", the same framing
// worstDisk(snap.Disks) uses for the instantaneous case (evalCount), applied
// here across the window instead of a single point in time.
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

// evalAggSeries implements avg|max|min(<sel>, <metric>) <op> <num>: each
// selector-matching node's own AVERAGE of metric over the `for` window (a node
// with no points in the window is excluded), then the rule's aggregation
// (avg/max/min) applied across those averages. If NO node has any data the
// result is "no data": it neither fires nor recovers, and holds the previous
// state. Self contributes its local-store average via ruleSelfSource.Store; a
// nil Store (or a backend that never collected this metric) excludes self.
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

// evalAbsent implements absent(<sel>, <dur>): fires when no selector-matching
// node (including none matching at all) has a LastSeen within AbsentFor of
// now, but only once the master itself has been up at least AbsentFor; during
// that window condTrue is forced false, not "no data" (the state is known:
// deliberately not yet judged). Self contributes `now` as its LastSeen
// whenever it matches sel, so it can never itself make an absent() rule fire.
// value is the seconds since the most recently seen matching node last
// reported (or since the master started, if none has EVER reported): a node
// whose LastSeen is genuinely 0 must not be read as "reported at the Unix
// epoch", which would make value a nonsensical ~55-year number.
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

// SetRules wires the aggregate-rule evaluator, called once from startMaster
// after both the engine and its data sources exist. A nil e.rules makes
// TickRules/RuleStates/ruleStillFiring no-ops. self is the master's own node
// data, so `all`/`node:<glob>` selectors include it too (see ruleSelfSource).
func (e *fleetAlertEngine) SetRules(reg *fleet.Registry, sink *replicaSink, tracker *fleet.Tracker, started time.Time, self ruleSelfSource) {
	e.rules = newFleetRuleEvaluator(reg, sink, tracker, started, self)
}

// TickRules evaluates every configured aggregate rule at most once every
// ruleTickInterval, submitting a fire/recover through Submit, like any other
// master-own alert, for every rule whose firing state just changed.
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
// "fleet:rule:<name>", title "rule <name>: <expr> (value <v>)", severity from
// the rule.
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

// ruleStillFiring implements stillActive for rules: false if the rule no longer
// exists in the live config or its in-memory state is not firing, so an
// orphaned "fleet:rule:<name>" incident (open across a master restart that
// removed or resolved the rule) recovers via checkOrphanedIncidents instead of
// paging forever.
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
