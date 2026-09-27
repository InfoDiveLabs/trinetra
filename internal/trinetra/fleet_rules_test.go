package trinetra

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// ---- grammar: parser round-trips ----------------------------------------

func TestParseRuleExprValidForms(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want ruleExpr
	}{
		{
			name: "count",
			expr: "count(tag:web, cpu > 90) >= 3 for 5m",
			want: ruleExpr{Kind: "count", Sel: ruleSelector{Kind: "tag", Value: "web"}, Metric: "cpu",
				InnerOp: ">", InnerNum: 90, OuterOp: ">=", OuterNum: 3, ForDur: 5 * time.Minute},
		},
		{
			name: "avg",
			expr: "avg(tag:db, mem) > 85 for 10m",
			want: ruleExpr{Kind: "avg", Sel: ruleSelector{Kind: "tag", Value: "db"}, Metric: "mem",
				OuterOp: ">", OuterNum: 85, ForDur: 10 * time.Minute},
		},
		{
			name: "max with node glob",
			expr: "max(node:web*, cpu) >= 95 for 2m",
			want: ruleExpr{Kind: "max", Sel: ruleSelector{Kind: "node", Value: "web*"}, Metric: "cpu",
				OuterOp: ">=", OuterNum: 95, ForDur: 2 * time.Minute},
		},
		{
			name: "min with all selector",
			expr: "min(all, load1) <= 0.5 for 1m",
			want: ruleExpr{Kind: "min", Sel: ruleSelector{Kind: "all"}, Metric: "load1",
				OuterOp: "<=", OuterNum: 0.5, ForDur: time.Minute},
		},
		{
			name: "online",
			expr: "online(tag:web) < 2 for 1m",
			want: ruleExpr{Kind: "online", Sel: ruleSelector{Kind: "tag", Value: "web"},
				OuterOp: "<", OuterNum: 2, ForDur: time.Minute},
		},
		{
			name: "absent",
			expr: "absent(tag:backup, 15m)",
			want: ruleExpr{Kind: "absent", Sel: ruleSelector{Kind: "tag", Value: "backup"}, AbsentFor: 15 * time.Minute},
		},
		{
			name: "equality operators and node exact id selector",
			expr: "count(node:n-abc123, disk == 100) != 0 for 1m",
			want: ruleExpr{Kind: "count", Sel: ruleSelector{Kind: "node", Value: "n-abc123"}, Metric: "disk",
				InnerOp: "==", InnerNum: 100, OuterOp: "!=", OuterNum: 0, ForDur: time.Minute},
		},
		{
			name: "temp metric and swap metric",
			expr: "count(all, temp > 80) >= 1 for 1m",
			want: ruleExpr{Kind: "count", Sel: ruleSelector{Kind: "all"}, Metric: "temp",
				InnerOp: ">", InnerNum: 80, OuterOp: ">=", OuterNum: 1, ForDur: time.Minute},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRuleExpr(tt.expr)
			if err != nil {
				t.Fatalf("parseRuleExpr(%q) = %v", tt.expr, err)
			}
			if *got != tt.want {
				t.Fatalf("parseRuleExpr(%q) = %+v, want %+v", tt.expr, *got, tt.want)
			}
		})
	}
}

// TestParseRuleExprBadInputs covers the task-7 ruling's "6+ bad inputs with
// position-bearing errors": every case's error must be a *ruleParseError
// whose Pos matches wantPos exactly (computed via strings.Index against the
// expression itself, so a test typo can never silently assert the wrong
// position).
func TestParseRuleExprBadInputs(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantAt  string // the substring whose start position the error must name
		wantMsg string // substring the error message must contain
	}{
		{name: "missing comma after selector", expr: "count(tag:web cpu > 90) >= 3 for 5m", wantAt: "cpu", wantMsg: "expected ','"},
		{name: "unknown metric", expr: "count(tag:web, bogus > 90) >= 3 for 5m", wantAt: "bogus", wantMsg: `unknown metric "bogus"`},
		{name: "single equals is not an operator", expr: "count(tag:web, cpu = 90) >= 3 for 5m", wantAt: "= 90", wantMsg: `expected "=="`},
		{name: "non-numeric threshold", expr: "count(tag:web, cpu > ninety) >= 3 for 5m", wantAt: "ninety", wantMsg: `invalid number "ninety"`},
		{name: "unknown function", expr: "banana(tag:web) > 1 for 1m", wantAt: "banana", wantMsg: `unknown function "banana"`},
		{name: "for duration below the 1m minimum", expr: "count(tag:web, cpu > 90) >= 3 for 30s", wantAt: "30s", wantMsg: "at least 1m"},
		{name: "invalid selector", expr: "count(bogus:web, cpu > 90) >= 3 for 5m", wantAt: "bogus:web", wantMsg: `invalid selector "bogus:web"`},
		{name: "missing closing paren", expr: "count(tag:web, cpu > 90 >= 3 for 5m", wantAt: ">= 3", wantMsg: "expected ')'"},
		{name: "non-integer outer threshold", expr: "online(tag:web) < 1.5 for 1m", wantAt: "1.5", wantMsg: `invalid integer "1.5"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseRuleExpr(tt.expr)
			if err == nil {
				t.Fatalf("parseRuleExpr(%q): expected an error", tt.expr)
			}
			pe, ok := err.(*ruleParseError)
			if !ok {
				t.Fatalf("parseRuleExpr(%q) error type = %T, want *ruleParseError", tt.expr, err)
			}
			wantPos := strings.Index(tt.expr, tt.wantAt)
			if wantPos < 0 {
				t.Fatalf("test bug: %q not found in %q", tt.wantAt, tt.expr)
			}
			if pe.Pos != wantPos {
				t.Fatalf("parseRuleExpr(%q) error pos = %d, want %d (error: %v)", tt.expr, pe.Pos, wantPos, err)
			}
			if !strings.Contains(pe.Msg, tt.wantMsg) {
				t.Fatalf("parseRuleExpr(%q) error = %q, want to contain %q", tt.expr, pe.Msg, tt.wantMsg)
			}
			// Every error names its position exactly per the ruling's own
			// example format ("expected ',' at 12").
			if !strings.HasSuffix(err.Error(), " at "+itoa(pe.Pos)) {
				t.Fatalf("parseRuleExpr(%q) error string = %q, want it to end with ' at %d'", tt.expr, err.Error(), pe.Pos)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// ---- compare(): every operator -------------------------------------------

func TestCompareEveryOperator(t *testing.T) {
	tests := []struct {
		op   string
		v, t float64
		want bool
	}{
		{">", 5, 3, true}, {">", 3, 5, false},
		{">=", 5, 5, true}, {">=", 4, 5, false},
		{"<", 3, 5, true}, {"<", 5, 3, false},
		{"<=", 5, 5, true}, {"<=", 6, 5, false},
		{"==", 5, 5, true}, {"==", 5, 6, false},
		{"!=", 5, 6, true}, {"!=", 5, 5, false},
	}
	for _, tt := range tests {
		if got := compare(tt.v, tt.op, tt.t); got != tt.want {
			t.Errorf("compare(%v, %q, %v) = %v, want %v", tt.v, tt.op, tt.t, got, tt.want)
		}
	}
}

// ---- validateRules / validateAlertingConfig hook --------------------------

func TestValidateRulesNameAndSeverity(t *testing.T) {
	base := core.AggregateRule{Name: "high-cpu_1", Expr: "count(all, cpu > 90) >= 1 for 1m"}

	if err := validateRules([]core.AggregateRule{base}); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}

	bad := base
	bad.Name = "Not Valid!"
	if err := validateRules([]core.AggregateRule{bad}); err == nil {
		t.Fatal("expected an error for an invalid rule name")
	}

	dup := []core.AggregateRule{base, base}
	if err := validateRules(dup); err == nil {
		t.Fatal("expected an error for a duplicate rule name")
	}

	badExpr := base
	badExpr.Expr = "not a valid expr"
	if err := validateRules([]core.AggregateRule{badExpr}); err == nil {
		t.Fatal("expected an error for an unparseable expr")
	}

	badSev := base
	badSev.Severity = "urgent"
	if err := validateRules([]core.AggregateRule{badSev}); err == nil {
		t.Fatal("expected an error for an invalid severity")
	}

	okSev := base
	okSev.Severity = "critical"
	if err := validateRules([]core.AggregateRule{okSev}); err != nil {
		t.Fatalf("valid severity rejected: %v", err)
	}
}

// TestValidateAlertingConfigAcceptsRulesOnlyConfig proves a config that only
// sets Rules (no routes/policies/default_policy) validates without being
// forced to also supply routing -- validateAlertingConfig's "totally empty
// routing" shortcut must still run rule validation, not skip it outright.
func TestValidateAlertingConfigAcceptsRulesOnlyConfig(t *testing.T) {
	cfg := core.AlertingConfig{Rules: []core.AggregateRule{{Name: "r1", Expr: "absent(tag:x, 5m)"}}}
	if err := validateAlertingConfig(cfg, allChannelsValid); err != nil {
		t.Fatalf("rules-only config rejected: %v", err)
	}
	cfg.Rules[0].Name = "Bad Name"
	if err := validateAlertingConfig(cfg, allChannelsValid); err == nil {
		t.Fatal("expected the bad rule name to be rejected even with no routing set")
	}
}

// ---- evaluation fixture ---------------------------------------------------

// ruleFixture wires a fleetAlertEngine with rules, routing and silences all
// active, over real on-disk registry/tracker/sink/incidents/alerting/silence
// stores (like production), so a test can drive TickRules off a fake clock
// and assert exactly what fired/recovered/delivered.
type ruleFixture struct {
	mu          sync.Mutex
	deliveredTo []namedDelivery

	dir       string
	reg       *fleet.Registry
	tracker   *fleet.Tracker
	sink      *replicaSink
	incidents *incidentStore
	alerting  *alertingStore
	silences  *silenceStore
	engine    *fleetAlertEngine
	now       time.Time
}

func newRuleFixture(t *testing.T) *ruleFixture {
	t.Helper()
	disableGroupWaitForTest(t)
	dir := t.TempDir()
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute))
	sink := newReplicaSink(filepath.Join(dir, "nodes"), StoreOptions{}, nil)
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	alerting, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	silences, err := loadSilenceStore(filepath.Join(dir, "silences.json"))
	if err != nil {
		t.Fatal(err)
	}

	rf := &ruleFixture{dir: dir, reg: reg, tracker: tracker, sink: sink, incidents: incidents,
		alerting: alerting, silences: silences, now: time.Unix(1_700_000_000, 0)}
	rf.engine = newFleetAlertEngine(
		func() time.Time { return rf.now },
		func(Alert) bool { return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents,
	)
	rf.engine.SetSilences(silences, func(id string) (string, []string) {
		if n, ok := reg.Get(id); ok {
			return n.Name, n.Tags
		}
		return id, nil
	})
	rf.engine.SetRouting(alerting, func(a Alert, channels []string) bool {
		rf.mu.Lock()
		defer rf.mu.Unlock()
		rf.deliveredTo = append(rf.deliveredTo, namedDelivery{a, channels})
		return true
	}, func(a Alert, channels []string) bool { return true })
	rf.engine.SetRules(reg, sink, tracker, rf.now)
	return rf
}

func (rf *ruleFixture) waitIdle() { rf.engine.waitIdleForTest() }

func (rf *ruleFixture) setRules(t *testing.T, rules ...core.AggregateRule) {
	t.Helper()
	if _, err := rf.alerting.Set(core.AlertingConfig{Rules: rules}, allChannelsValid); err != nil {
		t.Fatal(err)
	}
}

func (rf *ruleFixture) addNode(t *testing.T, name string, tags []string) fleet.Node {
	t.Helper()
	id, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	n := fleet.Node{ID: id, Name: name, Tags: tags, Joined: rf.now.Unix()}
	if err := rf.reg.Add(n); err != nil {
		t.Fatal(err)
	}
	got, _ := rf.reg.Get(id)
	return got
}

// setOnline marks id as having just made contact, at rf.now.
func (rf *ruleFixture) setOnline(id string) {
	rf.tracker.Seen(id, rf.now.Unix(), -1)
	rf.tracker.Evaluate(rf.now.Unix())
}

// setSnapshot posts a live snapshot for id, exactly like a child's fast-tick
// Live() call.
func (rf *ruleFixture) setSnapshot(t *testing.T, id string, snap Snapshot) {
	t.Helper()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := rf.sink.Live(id, fleet.LiveUpdate{SentAt: rf.now.Unix(), Snapshot: b}); err != nil {
		t.Fatal(err)
	}
}

// appendSeriesPoint appends one 1m-resolution rollup point directly (the
// same call fleet_replica.go's gap filler uses), so a series-based rule can
// be tested without going through raw ingest + downsample.
func (rf *ruleFixture) appendSeriesPoint(t *testing.T, id, metric string, ts int64, v float64) {
	t.Helper()
	n, err := rf.sink.node(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.AppendRollup(metric, Point{TS: ts, Min: v, Avg: v, Max: v}); err != nil {
		t.Fatal(err)
	}
}

func (rf *ruleFixture) tick() {
	rf.engine.TickRules(rf.now)
	rf.waitIdle()
}

func (rf *ruleFixture) advance(d time.Duration) { rf.now = rf.now.Add(d) }

func (rf *ruleFixture) deliveredCount() int {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return len(rf.deliveredTo)
}

func (rf *ruleFixture) lastDelivered() namedDelivery {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.deliveredTo[len(rf.deliveredTo)-1]
}

func (rf *ruleFixture) ruleState(t *testing.T, name string) core.RuleState {
	t.Helper()
	for _, s := range rf.engine.RuleStates() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no rule state for %q", name)
	return core.RuleState{}
}

// ---- count(): sustained `for`, then recover -------------------------------

func TestRuleCountSustainedForThenRecovers(t *testing.T) {
	rf := newRuleFixture(t)
	web1 := rf.addNode(t, "web1", []string{"web"})
	web2 := rf.addNode(t, "web2", []string{"web"})
	rf.setOnline(web1.ID)
	rf.setOnline(web2.ID)
	rf.setSnapshot(t, web1.ID, Snapshot{CPU: 95})
	rf.setSnapshot(t, web2.ID, Snapshot{CPU: 10})
	rf.setRules(t, core.AggregateRule{Name: "hot-web", Expr: "count(tag:web, cpu > 90) >= 1 for 5m", Severity: "critical"})

	// t0: condition becomes true (1 hot node), but the `for` window has not
	// elapsed yet -- must not fire.
	rf.tick()
	if st := rf.ruleState(t, "hot-web"); st.Firing || st.Since == 0 {
		t.Fatalf("hot-web state after t0 = %+v, want not firing but since set", st)
	}
	if rf.deliveredCount() != 0 {
		t.Fatalf("delivered too early: %d", rf.deliveredCount())
	}

	// t0+150s: still under 5m sustained.
	rf.advance(150 * time.Second)
	rf.tick()
	if st := rf.ruleState(t, "hot-web"); st.Firing {
		t.Fatalf("hot-web fired too early: %+v", st)
	}

	// t0+330s: 5m30s sustained -- fires now.
	rf.advance(180 * time.Second)
	rf.tick()
	st := rf.ruleState(t, "hot-web")
	if !st.Firing {
		t.Fatalf("hot-web did not fire after 5m30s sustained: %+v", st)
	}
	if st.Value != 1 {
		t.Fatalf("hot-web value = %v, want 1", st.Value)
	}
	if rf.deliveredCount() != 1 {
		t.Fatalf("delivered = %d, want 1", rf.deliveredCount())
	}
	fire := rf.lastDelivered()
	if fire.a.Key != "fleet:rule:hot-web" || fire.a.Kind != "fire" || fire.a.Severity != SevCritical {
		t.Fatalf("fire alert = %+v", fire.a)
	}
	if !strings.Contains(fire.a.Title, "rule hot-web: count(tag:web, cpu > 90) >= 1 for 5m (value 1)") {
		t.Fatalf("fire title = %q", fire.a.Title)
	}

	// Recover: cool the hot node down, tick again (>=30s later).
	rf.setSnapshot(t, web1.ID, Snapshot{CPU: 5})
	rf.advance(30 * time.Second)
	rf.tick()
	st = rf.ruleState(t, "hot-web")
	if st.Firing {
		t.Fatalf("hot-web still firing after recovering: %+v", st)
	}
	if rf.deliveredCount() != 2 {
		t.Fatalf("delivered after recover = %d, want 2", rf.deliveredCount())
	}
	recover := rf.lastDelivered()
	if recover.a.Key != "fleet:rule:hot-web" || recover.a.Kind != "recover" {
		t.Fatalf("recover alert = %+v", recover.a)
	}
}

// ---- avg() over series: two nodes, one excluded for lacking data ----------

func TestRuleAvgSeriesTwoNodesExcludesNoData(t *testing.T) {
	rf := newRuleFixture(t)
	db1 := rf.addNode(t, "db1", []string{"db"})
	db2 := rf.addNode(t, "db2", []string{"db"})
	db3 := rf.addNode(t, "db3", []string{"db"}) // no series data at all: must be excluded
	for _, id := range []string{db1.ID, db2.ID, db3.ID} {
		rf.setOnline(id)
	}
	base := rf.now.Unix()
	// db1 averages 90, db2 averages 80 over the 10m window -> avg across
	// nodes = 85.
	for _, ts := range []int64{base - 540, base - 480, base - 420} {
		rf.appendSeriesPoint(t, db1.ID, "mem", ts, 90)
		rf.appendSeriesPoint(t, db2.ID, "mem", ts, 80)
	}
	rf.setRules(t, core.AggregateRule{Name: "db-mem", Expr: "avg(tag:db, mem) > 84 for 10m"})

	rf.tick()
	st := rf.ruleState(t, "db-mem")
	if !st.HasValue || st.NoData {
		t.Fatalf("db-mem state = %+v, want a value (db3 excluded, not no-data)", st)
	}
	if st.Value != 85 {
		t.Fatalf("db-mem value = %v, want 85", st.Value)
	}
	// Sustained for 0s so far, but the fixture's clock has not advanced past
	// the 10m for-window since the condition became true; firing follows the
	// same sustain state machine as count() (tested above), so here we only
	// assert the aggregate value itself and that db3's absence didn't turn
	// the whole rule into "no data".
}

// ---- all-no-data holds the previous state ---------------------------------

func TestRuleAllNoDataHoldsPreviousState(t *testing.T) {
	rf := newRuleFixture(t)
	db1 := rf.addNode(t, "db1", []string{"db"})
	rf.setOnline(db1.ID)
	rf.setRules(t, core.AggregateRule{Name: "db-mem", Expr: "avg(tag:db, mem) > 50 for 1m"})

	// No series data at all yet: the whole rule is "no data".
	rf.tick()
	st := rf.ruleState(t, "db-mem")
	if !st.NoData || st.Firing || st.Since != 0 {
		t.Fatalf("db-mem state with no data at all = %+v, want NoData with nothing firing", st)
	}

	// Still no data on the next tick: must hold, not spuriously flip
	// anything (and definitely never fire).
	rf.advance(60 * time.Second)
	rf.tick()
	st2 := rf.ruleState(t, "db-mem")
	if !st2.NoData || st2.Firing {
		t.Fatalf("db-mem state on second no-data tick = %+v, want still NoData/not firing", st2)
	}
	if rf.deliveredCount() != 0 {
		t.Fatalf("no-data ticks must never deliver anything: delivered = %d", rf.deliveredCount())
	}
}

// ---- online() uses the tracker ---------------------------------------------

func TestRuleOnlineUsesTracker(t *testing.T) {
	rf := newRuleFixture(t)
	web1 := rf.addNode(t, "web1", []string{"web"})
	web2 := rf.addNode(t, "web2", []string{"web"})
	web3 := rf.addNode(t, "web3", []string{"web"})
	rf.setOnline(web1.ID) // only one of three online -> online(tag:web) < 2 is true
	rf.setRules(t, core.AggregateRule{Name: "web-quorum", Expr: "online(tag:web) < 2 for 1m"})

	rf.tick()
	if st := rf.ruleState(t, "web-quorum"); st.Value != 1 || st.Firing {
		t.Fatalf("web-quorum after t0 = %+v, want value 1, not yet firing", st)
	}

	rf.advance(90 * time.Second)
	rf.tick()
	st := rf.ruleState(t, "web-quorum")
	if !st.Firing {
		t.Fatalf("web-quorum did not fire after 90s sustained: %+v", st)
	}

	// Bring web2 online too (both nodes freshly heartbeat, since
	// Tracker.Evaluate reclassifies every tracked node, not just the one just
	// Seen): 2 >= 2, condition clears, recovers.
	rf.setOnline(web1.ID)
	rf.setOnline(web2.ID)
	_ = web3
	rf.advance(30 * time.Second)
	rf.tick()
	if st := rf.ruleState(t, "web-quorum"); st.Firing {
		t.Fatalf("web-quorum still firing once quorum restored: %+v", st)
	}
	if rf.deliveredCount() != 2 { // one fire, one recover
		t.Fatalf("delivered = %d, want 2", rf.deliveredCount())
	}
}

// ---- absent() and its blind window -----------------------------------------

func TestRuleAbsentBlindWindow(t *testing.T) {
	rf := newRuleFixture(t)
	backup := rf.addNode(t, "backup1", []string{"backup"})
	// backup1's LastSeen defaults to 0 (never reported since master start in
	// this fixture) -- absent should still be forced false during the blind
	// window itself.
	_ = backup
	rf.setRules(t, core.AggregateRule{Name: "backup-missing", Expr: "absent(tag:backup, 15m)"})

	// Still inside the 15m blind window (fixture clock == master start).
	rf.tick()
	if st := rf.ruleState(t, "backup-missing"); st.Firing {
		t.Fatalf("absent fired inside its own blind window: %+v", st)
	}

	// Past the blind window, still no report ever: fires immediately (no
	// `for` clause -- absent fires the tick its condition first reads true).
	rf.advance(16 * time.Minute)
	rf.tick()
	st := rf.ruleState(t, "backup-missing")
	if !st.Firing {
		t.Fatalf("absent did not fire once past the blind window with no report: %+v", st)
	}

	// The node reports: recovers.
	if err := rf.reg.Update(backup.ID, func(n *fleet.Node) error { n.LastSeen = rf.now.Unix(); return nil }); err != nil {
		t.Fatal(err)
	}
	rf.advance(30 * time.Second)
	rf.tick()
	if st := rf.ruleState(t, "backup-missing"); st.Firing {
		t.Fatalf("absent still firing after the node reported: %+v", st)
	}
}

// TestRuleAbsentFiresWhenNoNodeMatchesAtAll covers the "including the case
// where no node matches the selector at all" ruling.
func TestRuleAbsentFiresWhenNoNodeMatchesAtAll(t *testing.T) {
	rf := newRuleFixture(t)
	rf.setRules(t, core.AggregateRule{Name: "no-such-tag", Expr: "absent(tag:nonexistent, 5m)"})
	rf.advance(6 * time.Minute)
	rf.tick()
	if st := rf.ruleState(t, "no-such-tag"); !st.Firing {
		t.Fatalf("absent with zero matching nodes did not fire: %+v", st)
	}
}

// ---- a rule deleted while firing recovers ---------------------------------

func TestRuleDeletedWhileFiringRecovers(t *testing.T) {
	rf := newRuleFixture(t)
	node := rf.addNode(t, "web1", []string{"web"})
	rf.setOnline(node.ID)
	rf.setSnapshot(t, node.ID, Snapshot{CPU: 95})
	rf.setRules(t, core.AggregateRule{Name: "hot-web", Expr: "count(tag:web, cpu > 90) >= 1 for 1m"})

	rf.tick()
	rf.advance(90 * time.Second)
	rf.tick()
	if st := rf.ruleState(t, "hot-web"); !st.Firing {
		t.Fatalf("hot-web should be firing before deletion: %+v", st)
	}
	if rf.deliveredCount() != 1 {
		t.Fatalf("delivered before deletion = %d, want 1", rf.deliveredCount())
	}

	// Delete the rule while it's firing.
	rf.setRules(t) // empty Rules
	rf.advance(30 * time.Second)
	rf.tick()
	if rf.deliveredCount() != 2 {
		t.Fatalf("delivered after deleting a firing rule = %d, want 2 (fire + recover)", rf.deliveredCount())
	}
	recover := rf.lastDelivered()
	if recover.a.Key != "fleet:rule:hot-web" || recover.a.Kind != "recover" {
		t.Fatalf("expected a recover for the deleted rule, got %+v", recover.a)
	}
}

// ---- orphan recover via stillActive ----------------------------------------

// TestRuleOrphanRecoverViaStillActive proves masterLoop.stillActive's
// "fleet:rule:<name>" case recovers a stale open incident after a restart:
// the fresh engine's in-memory rule state starts over (task-7 ruling), so
// even though the rule still exists and its condition is (from a fresh
// evaluator's point of view) unknown, stillActive must say "not still
// active" -- there is nothing in the restarted process that remembers it
// was firing.
func TestRuleOrphanRecoverViaStillActive(t *testing.T) {
	rf := newRuleFixture(t)
	node := rf.addNode(t, "web1", []string{"web"})
	rf.setOnline(node.ID)
	rf.setSnapshot(t, node.ID, Snapshot{CPU: 95})
	rf.setRules(t, core.AggregateRule{Name: "hot-web", Expr: "count(tag:web, cpu > 90) >= 1 for 1m"})
	rf.tick()
	rf.advance(90 * time.Second)
	rf.tick()
	if st := rf.ruleState(t, "hot-web"); !st.Firing {
		t.Fatalf("hot-web should be firing: %+v", st)
	}

	// Simulate a restart: a brand new engine/evaluator over the SAME
	// incidents/alerting stores (on disk), with no memory of hot-web ever
	// having fired.
	freshIncidents, err := loadIncidentStore(filepath.Join(rf.dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	freshEngine := newFleetAlertEngine(func() time.Time { return rf.now }, func(Alert) bool { return true },
		func(string, fleet.Frame) bool { return true }, func(string) bool { return true }, freshIncidents)
	freshEngine.SetRouting(rf.alerting, func(Alert, []string) bool { return true }, func(Alert, []string) bool { return true })
	freshEngine.SetRules(rf.reg, rf.sink, rf.tracker, rf.now)

	loop := newMasterLoop(rf.reg, rf.tracker, rf.sink, freshEngine, fleetDeps{alert: func(Alert) {}}, rf.now)
	if loop.stillActive(ruleAlertKey("hot-web"), fleet.Evaluation{}) {
		t.Fatal("stillActive should report false for a rule with no memory of firing since restart")
	}

	// And once the rule is removed from the live config entirely,
	// stillActive must say false regardless of any in-memory firing state.
	rf.setRules(t) // remove hot-web
	if loop.stillActive(ruleAlertKey("hot-web"), fleet.Evaluation{}) {
		t.Fatal("stillActive should report false for a rule that no longer exists")
	}
}

// ---- a rule alert goes through silence and routing like any other alert ---

func TestRuleAlertSilencedIsNotDelivered(t *testing.T) {
	rf := newRuleFixture(t)
	node := rf.addNode(t, "web1", []string{"web"})
	rf.setOnline(node.ID)
	rf.setSnapshot(t, node.ID, Snapshot{CPU: 95})
	rf.setRules(t, core.AggregateRule{Name: "hot-web", Expr: "count(tag:web, cpu > 90) >= 1 for 1m"})

	if _, err := rf.silences.Create(core.Silence{
		Matchers: []core.Matcher{{Rule: ruleAlertKey("hot-web")}},
		Start:    rf.now.Unix() - 10,
		End:      rf.now.Unix() + int64(time.Hour/time.Second),
		Author:   "test",
	}); err != nil {
		t.Fatal(err)
	}

	rf.tick()
	rf.advance(90 * time.Second)
	rf.tick()

	if rf.deliveredCount() != 0 {
		t.Fatalf("a silenced rule alert must never be delivered: delivered = %d", rf.deliveredCount())
	}
	// It must still be RECORDED (suppressed, not dropped) -- global
	// constraints: "suppressed alerts are always recorded with the reason".
	inc, ok := rf.incidents.FindByAlertKey(ruleAlertKey("hot-web"))
	if !ok {
		t.Fatal("silenced rule alert was not recorded as an incident at all")
	}
	if inc.State != "suppressed" {
		t.Fatalf("incident state = %q, want suppressed", inc.State)
	}
}

func TestRuleAlertRoutedNormallyWhenNotSilenced(t *testing.T) {
	rf := newRuleFixture(t)
	node := rf.addNode(t, "web1", []string{"web"})
	rf.setOnline(node.ID)
	rf.setSnapshot(t, node.ID, Snapshot{CPU: 95})
	rf.setRules(t, core.AggregateRule{Name: "hot-web", Expr: "count(tag:web, cpu > 90) >= 1 for 1m"})

	rf.tick()
	rf.advance(90 * time.Second)
	rf.tick()

	if rf.deliveredCount() != 1 {
		t.Fatalf("an unsilenced rule alert must be delivered exactly once: delivered = %d", rf.deliveredCount())
	}
}
