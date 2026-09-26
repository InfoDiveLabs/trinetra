package trinetra

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// --- Matcher / validation ----------------------------------------------

// idOfDB1/idOfWeb1/idOfApp1 are placeholder node ids distinct from any test
// node's NAME, so a test asserting on name-glob behaviour can never
// accidentally pass via the separate exact-id-match branch instead.
const (
	idOfDB1  = "id-of-db1-node"
	idOfWeb1 = "id-of-web1-node"
	idOfApp1 = "id-of-app1-node"
)

func TestMatcherMatchesGlobsExactFieldsAndEmptyMeansAny(t *testing.T) {
	m := core.Matcher{Node: "db*", Rule: "cpu*", Severity: "critical", Tag: "web"}
	if !m.Matches(idOfDB1, "db1", []string{"web", "prod"}, "cpu_pct", "critical") {
		t.Fatal("want match: node glob, rule glob, tag present, severity exact")
	}
	if m.Matches(idOfApp1, "app1", []string{"web"}, "cpu_pct", "critical") {
		t.Fatal("node glob must reject a non-matching node")
	}
	if m.Matches(idOfDB1, "db1", []string{"web"}, "mem_pct", "critical") {
		t.Fatal("rule glob must reject a non-matching rule")
	}
	if m.Matches(idOfDB1, "db1", []string{"web"}, "cpu_pct", "warning") {
		t.Fatal("severity must be exact")
	}
	if m.Matches(idOfDB1, "db1", []string{"prod"}, "cpu_pct", "critical") {
		t.Fatal("tag must be present on the node")
	}

	empty := core.Matcher{}
	if !empty.Matches("id", "anything", nil, "anything", "anything") {
		t.Fatal("an entirely empty matcher must match anything")
	}
	ruleOnly := core.Matcher{Rule: "cpu*"}
	if !ruleOnly.Matches("some-id", "some-other-node", nil, "cpu_pct", "critical") {
		t.Fatal("a rule-only matcher must apply regardless of node")
	}
}

// TestMatcherNodeMatchesExactIDRegardlessOfName is the review round-2 item
// (c) test: Matcher.Node matches if it EXACTLY equals the node's internal
// id, even when it does NOT glob-match the node's current display name --
// giving a precise, rename-proof target.
func TestMatcherNodeMatchesExactIDRegardlessOfName(t *testing.T) {
	m := core.Matcher{Node: idOfDB1}
	if !m.Matches(idOfDB1, "totally-renamed", nil, "cpu_pct", "critical") {
		t.Fatal("an exact node id match must apply even though the name no longer globs it")
	}
	if m.Matches("some-other-id", "db1", nil, "cpu_pct", "critical") {
		t.Fatal("a different node's id must not match")
	}
	if !m.CouldApplyToNode(idOfDB1, "totally-renamed", nil) {
		t.Fatal("CouldApplyToNode must also recognize the exact-id match")
	}
}

// TestMatcherNodeWithRealHexIDIsNotAUsableGlob is the review round-3 minor:
// a real, generated node id (fleet.NewNodeID, 32 hex chars) pins that it can
// only ever match via the exact-id branch, never as a glob pattern -- hex
// characters have no special meaning to path.Match, so a real id used as
// Matcher.Node can never accidentally act as a wildcard against some other
// node's name or id.
func TestMatcherNodeWithRealHexIDIsNotAUsableGlob(t *testing.T) {
	id, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	m := core.Matcher{Node: id}
	if !m.Matches(id, "renamed-away", nil, "cpu_pct", "critical") {
		t.Fatal("must match its own exact id regardless of its current name")
	}
	if m.Matches("some-other-id", "web1", nil, "cpu_pct", "critical") {
		t.Fatal("a real hex id used as Node must not match an unrelated id/name")
	}
	if m.Matches("some-other-id", "totally-unrelated-but-longer-name", nil, "cpu_pct", "critical") {
		t.Fatal("a real hex id used as Node must not glob-match an unrelated name")
	}
}

func TestMatcherCouldApplyToNodeIgnoresRuleAndSeverity(t *testing.T) {
	m := core.Matcher{Rule: "cpu*", Severity: "critical"}
	if !m.CouldApplyToNode("any-id", "any-node", nil) {
		t.Fatal("a rule/severity-only matcher could apply to every node")
	}
	tagged := core.Matcher{Tag: "web"}
	if tagged.CouldApplyToNode("id-n1", "n1", []string{"db"}) {
		t.Fatal("a tag matcher must not apply to a node missing that tag")
	}
	if !tagged.CouldApplyToNode("id-n1", "n1", []string{"web"}) {
		t.Fatal("a tag matcher must apply to a node carrying that tag")
	}
}

func TestValidateMatchersRejectsEmptyOrMissing(t *testing.T) {
	if err := validateMatchers(nil); err == nil {
		t.Fatal("want error for no matchers")
	}
	if err := validateMatchers([]core.Matcher{{}}); err == nil {
		t.Fatal("want error for a completely empty matcher (matches everything)")
	}
	if err := validateMatchers([]core.Matcher{{Tag: "web"}}); err != nil {
		t.Fatalf("a single non-empty matcher must be accepted: %v", err)
	}
}

// --- maintenance window expansion ---------------------------------------

func TestMaintenanceActiveAtCrossesMidnightUTC(t *testing.T) {
	m := core.Maintenance{
		Name: "nightly", Weekdays: []int{1}, // Monday
		From: "22:00", To: "02:00", TZ: "UTC",
	}
	// 2024-01-01 is a Monday.
	before := time.Date(2024, 1, 1, 21, 59, 0, 0, time.UTC)
	if maintenanceActiveAt(m, before) {
		t.Fatal("must not be active before the window starts")
	}
	duringMonday := time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC)
	if !maintenanceActiveAt(m, duringMonday) {
		t.Fatal("must be active shortly after 22:00 Monday")
	}
	duringTuesday := time.Date(2024, 1, 2, 1, 30, 0, 0, time.UTC)
	if !maintenanceActiveAt(m, duringTuesday) {
		t.Fatal("must still be active just after midnight, into Tuesday (crossing midnight)")
	}
	after := time.Date(2024, 1, 2, 2, 1, 0, 0, time.UTC)
	if maintenanceActiveAt(m, after) {
		t.Fatal("must not be active once past 02:00 Tuesday")
	}
}

func TestMaintenanceActiveAtNonUTCTimezone(t *testing.T) {
	m := core.Maintenance{
		Name: "ist-window", Weekdays: []int{1}, // Monday, in Asia/Kolkata (UTC+5:30)
		From: "22:00", To: "23:00", TZ: "Asia/Kolkata",
	}
	// 22:00 IST Monday = 16:30 UTC Monday.
	activeUTC := time.Date(2024, 1, 1, 16, 45, 0, 0, time.UTC)
	if !maintenanceActiveAt(m, activeUTC) {
		t.Fatal("want active: 16:45 UTC is 22:15 IST on the same Monday")
	}
	notYetUTC := time.Date(2024, 1, 1, 16, 0, 0, 0, time.UTC)
	if maintenanceActiveAt(m, notYetUTC) {
		t.Fatal("want inactive: 16:00 UTC is 21:30 IST, before the window")
	}
}

func TestMaintenanceOccurrencesInRangeExpandsNext24h(t *testing.T) {
	m := core.Maintenance{Name: "w", Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, From: "01:00", To: "03:00", TZ: "UTC"}
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	occs := maintenanceOccurrencesInRange(m, from, from.Add(24*time.Hour))
	if len(occs) != 1 {
		t.Fatalf("occurrences = %+v, want 1 (only today's 01:00-03:00 fits a 24h forward window from midnight)", occs)
	}
}

// --- review round 1, item 2: DST -------------------------------------------

// TestMaintenanceOccurrenceFallBackLastsExactlyOneHour is the review round-1
// item 2 regression test: 2024-11-03 is the US fall-back day in
// America/New_York (clocks go from 02:00 EDT back to 01:00 EST, so the
// 01:00-02:00 hour occurs twice in real time). Computing the occurrence's
// end independently via time.Date (the old code) would double this window
// to 2 real hours; computing it as start.Add(wallDuration) keeps it at
// exactly 1h regardless.
func TestMaintenanceOccurrenceFallBackLastsExactlyOneHour(t *testing.T) {
	m := core.Maintenance{Name: "w", Weekdays: []int{0}, From: "01:00", To: "02:00", TZ: "America/New_York"} // Sunday
	from := time.Date(2024, 11, 3, 0, 0, 0, 0, time.UTC)                                                     // 2024-11-03 is a Sunday
	occs := maintenanceOccurrencesInRange(m, from, from.Add(24*time.Hour))
	if len(occs) != 1 {
		t.Fatalf("occurrences = %+v, want 1", occs)
	}
	if dur := time.Duration(occs[0].End-occs[0].Start) * time.Second; dur != time.Hour {
		t.Fatalf("duration across the fall-back transition = %s, want exactly 1h", dur)
	}
}

// TestMaintenanceOccurrenceSpringForwardDocumented documents the accepted
// trade-off on the other DST transition (review round 1, item 2): on
// 2024-03-10 in America/New_York (spring-forward: clocks jump from 02:00
// EST straight to 03:00 EDT, so wall-clock 02:00-02:59 never happens that
// day), a 01:30-02:30 window's real elapsed duration still comes out to
// exactly the configured 1h (start.Add(wallDuration) is immune to the gap),
// but its END lands on 03:30 local time, not 02:30 -- because 02:30 simply
// never existed that day, so "1h after 01:30" is 03:30 once the gap is
// accounted for.
func TestMaintenanceOccurrenceSpringForwardDocumented(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	m := core.Maintenance{Name: "w", Weekdays: []int{0}, From: "01:30", To: "02:30", TZ: "America/New_York"} // Sunday
	from := time.Date(2024, 3, 10, 0, 0, 0, 0, loc)                                                          // 2024-03-10 is a Sunday
	occs := maintenanceOccurrencesInRange(m, from, from.Add(24*time.Hour))
	if len(occs) != 1 {
		t.Fatalf("occurrences = %+v, want 1", occs)
	}
	if dur := time.Duration(occs[0].End-occs[0].Start) * time.Second; dur != time.Hour {
		t.Fatalf("duration across the spring-forward gap = %s, want exactly the configured 1h", dur)
	}
	endLocal := time.Unix(occs[0].End, 0).In(loc)
	if endLocal.Hour() != 3 || endLocal.Minute() != 30 {
		t.Fatalf("end local time = %s, want 03:30 (documented: lands past the gap, not at the nonexistent 02:30)", endLocal.Format("15:04"))
	}
}

// TestMaintenanceOccurrenceOwnedBySundayNotMonday is the review round-1 item
// 2 weekday-ownership test: a window crossing midnight is scheduled by the
// weekday of its START, never the day its End happens to land on.
// Weekdays=[Sunday] must produce the Sunday 22:00 -> Monday 02:00
// occurrence; it must NOT also (or instead) require Monday in Weekdays.
func TestMaintenanceOccurrenceOwnedBySundayNotMonday(t *testing.T) {
	m := core.Maintenance{Name: "w", Weekdays: []int{0}, From: "22:00", To: "02:00", TZ: "UTC"} // Sunday only
	from := time.Date(2024, 1, 7, 0, 0, 0, 0, time.UTC)                                         // 2024-01-07 is a Sunday
	until := time.Date(2024, 1, 9, 0, 0, 0, 0, time.UTC)
	occs := maintenanceOccurrencesInRange(m, from, until)
	if len(occs) != 1 {
		t.Fatalf("occurrences = %+v, want exactly 1: Weekdays=[Sunday] alone must produce it", occs)
	}
	start := time.Unix(occs[0].Start, 0).UTC()
	end := time.Unix(occs[0].End, 0).UTC()
	if start.Weekday() != time.Sunday {
		t.Fatalf("occurrence start weekday = %v, want Sunday (the window is owned by the day its start falls on)", start.Weekday())
	}
	if end.Weekday() != time.Monday {
		t.Fatalf("occurrence end weekday = %v, want Monday (crossing midnight lands the end there, but ownership stays with Sunday)", end.Weekday())
	}
}

// --- silenceStore ---------------------------------------------------------

func newTestSilenceStore(t *testing.T) *silenceStore {
	t.Helper()
	s, err := loadSilenceStore(filepath.Join(t.TempDir(), "silences.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSilenceStoreCreateValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "silences.json")
	s, err := loadSilenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(core.Silence{Matchers: nil, Start: 1000, End: 2000, Author: "cli"}); err == nil {
		t.Fatal("want error creating a silence with no matchers")
	}
	sil, err := s.Create(core.Silence{Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 2000, Author: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if sil.ID == "" {
		t.Fatal("want an assigned ID")
	}

	reloaded, err := loadSilenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.List()
	if len(got) != 1 || got[0].ID != sil.ID {
		t.Fatalf("reloaded silences = %+v, want the one just created", got)
	}
}

func TestSilenceStoreExpirePullsEndBack(t *testing.T) {
	s := newTestSilenceStore(t)
	sil, err := s.Create(core.Silence{Matchers: []core.Matcher{{Tag: "web"}}, Start: 1000, End: 1_000_000, Author: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Expire(sil.ID, 5000); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 1 || got[0].End != 5000 {
		t.Fatalf("silence = %+v, want End=5000", got)
	}
	// Expiring an already-past silence is a no-op, not an error.
	if err := s.Expire(sil.ID, 9999); err != nil {
		t.Fatal(err)
	}
	if s.List()[0].End != 5000 {
		t.Fatal("expiring again with a LATER now must not push End forward")
	}
	if err := s.Expire("nosuch", 1); err == nil {
		t.Fatal("want error expiring an unknown id")
	}
}

func TestSilenceStorePruneDropsOnlyLongExpired(t *testing.T) {
	s := newTestSilenceStore(t)
	now := int64(1_000_000)
	weekAgo := now - int64(silenceExpiredPruneAfter/time.Second) - 10
	recentlyExpired := now - 3600
	if _, err := s.Create(core.Silence{Matchers: []core.Matcher{{Tag: "old"}}, Start: weekAgo - 100, End: weekAgo}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(core.Silence{Matchers: []core.Matcher{{Tag: "recent"}}, Start: recentlyExpired - 100, End: recentlyExpired}); err != nil {
		t.Fatal(err)
	}
	s.Prune(now)
	got := s.List()
	if len(got) != 1 || got[0].Matchers[0].Tag != "recent" {
		t.Fatalf("after prune = %+v, want only the recently-expired one kept", got)
	}
}

func TestSilenceStoreSuppressedMatchesActiveSilenceAndMaintenance(t *testing.T) {
	s := newTestSilenceStore(t)
	if _, err := s.Create(core.Silence{Matchers: []core.Matcher{{Rule: "cpu*"}}, Start: 1000, End: 2000, Author: "cli"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Suppressed(1500, "id-n1", "n1", nil, "cpu_pct", "critical"); got == nil || !strings.HasPrefix(got.Reason, "silence ") {
		t.Fatalf("want suppressed by the active silence, got %+v", got)
	}
	if got := s.Suppressed(2500, "id-n1", "n1", nil, "cpu_pct", "critical"); got != nil {
		t.Fatalf("want not suppressed once the silence has ended, got %+v", got)
	}
	if got := s.Suppressed(1500, "id-n1", "n1", nil, "mem_pct", "critical"); got != nil {
		t.Fatalf("want not suppressed: rule doesn't match, got %+v", got)
	}

	if _, err := s.SaveMaintenance(core.Maintenance{
		Name: "nightly", Matchers: []core.Matcher{{Tag: "web"}}, Weekdays: []int{1}, From: "22:00", To: "02:00", TZ: "UTC",
	}); err != nil {
		t.Fatal(err)
	}
	duringMonday := time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC).Unix()
	if got := s.Suppressed(duringMonday, "id-n1", "n1", []string{"web"}, "mem_pct", "warning"); got == nil || !strings.HasPrefix(got.Reason, "maintenance ") {
		t.Fatalf("want suppressed by the active maintenance window, got %+v", got)
	}
	if got := s.Suppressed(duringMonday, "id-n1", "n1", []string{"db"}, "mem_pct", "warning"); got != nil {
		t.Fatalf("want not suppressed: node lacks the required tag, got %+v", got)
	}
}

func TestSilenceStoreSilencesForNodeFiltersByMatcherAndExpandsMaintenance(t *testing.T) {
	s := newTestSilenceStore(t)
	if _, err := s.Create(core.Silence{Matchers: []core.Matcher{{Tag: "web"}}, Start: 900, End: 2000, Author: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(core.Silence{Matchers: []core.Matcher{{Node: "db*"}}, Start: 900, End: 2000, Author: "cli"}); err != nil {
		t.Fatal(err)
	}
	// A separate tag so this maintenance window never leaks into the
	// web1/db1 assertions below.
	if _, err := s.SaveMaintenance(core.Maintenance{
		Name: "w", Matchers: []core.Matcher{{Tag: "ops"}}, Weekdays: []int{0, 1, 2, 3, 4, 5, 6}, From: "01:00", To: "03:00", TZ: "UTC",
	}); err != nil {
		t.Fatal(err)
	}

	forWeb := s.silencesForNode(1000, "id-web1", "web1", []string{"web"})
	if len(forWeb) != 1 || forWeb[0].Reason != "" && !strings.HasPrefix(forWeb[0].Reason, "silence ") {
		t.Fatalf("silences for web1 = %+v, want exactly the tag=web silence (not the node=db* one)", forWeb)
	}
	forDB := s.silencesForNode(1000, "id-db1", "db1", nil)
	if len(forDB) != 1 {
		t.Fatalf("silences for db1 = %+v, want exactly the node=db* silence", forDB)
	}
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	forOps := s.silencesForNode(now, "id-ops1", "ops1", []string{"ops"})
	maintCount := 0
	for _, p := range forOps {
		if strings.HasPrefix(p.Reason, "maintenance ") {
			maintCount++
		}
	}
	if maintCount != 1 {
		t.Fatalf("want the maintenance window expanded into (at least) one next-24h occurrence, got %d (%+v)", maintCount, forOps)
	}
	forOther := s.silencesForNode(now, "id-other1", "other1", nil)
	for _, p := range forOther {
		if strings.HasPrefix(p.Reason, "maintenance ") {
			t.Fatalf("a tag=ops maintenance window must not be pushed to a node without that tag: %+v", forOther)
		}
	}
}

// --- engine: suppression, unsilence-delivery, push -------------------------

func TestEngineSuppressesChildAlertButStillSendsReceipt(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0)) // well before the alert fires at 1000

	store := newTestSilenceStore(t)
	if _, err := store.Create(core.Silence{Matchers: []core.Matcher{{Rule: "cpu*"}}, Start: 0, End: ef.now.Unix() + 1000, Author: "cli"}); err != nil {
		t.Fatal(err)
	}
	ef.engine.SetSilences(store, func(id string) (string, []string) { return "box1", []string{"web"} })

	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", []string{"web"}, ev)
	ef.waitIdle()

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none: the alert is silenced", ef.delivered)
	}
	if got := len(ef.framesFor("n1", "receipt")); got != 1 {
		t.Fatalf("receipts = %d, want 1: a silenced child alert must still get a receipt so it doesn't fall back", got)
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || incs[0].State != "suppressed" {
		t.Fatalf("incidents = %+v, want one in state suppressed", incs)
	}
	found := false
	for _, e := range incs[0].Timeline {
		if e.Kind == "suppressed" && strings.HasPrefix(e.Detail, "silence ") && strings.Contains(e.Detail, "by cli") {
			found = true
		}
	}
	if !found {
		t.Fatalf("timeline = %+v, want a suppressed event 'silence <id> by cli'", incs[0].Timeline)
	}
}

func TestEngineSuppressesMasterOwnAlertLegLabeled(t *testing.T) {
	ef := newEngineFixture(t)
	store := newTestSilenceStore(t)
	if _, err := store.Create(core.Silence{Matchers: []core.Matcher{{Rule: "fleet:node:*"}}, Start: 0, End: ef.now.Unix() + 1000, Author: "cli"}); err != nil {
		t.Fatal(err)
	}
	ef.engine.SetSilences(store, nil)

	a := Alert{Key: "fleet:node:n1:down", Title: "n1 is down", Severity: SevCritical, Kind: "fire", Source: "fleet", Time: 1000}
	ef.engine.Submit(alertSource{}, a)
	ef.waitIdle()

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none", ef.delivered)
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || incs[0].State != "suppressed" {
		t.Fatalf("incidents = %+v, want one suppressed", incs)
	}
	found := false
	for _, e := range incs[0].Timeline {
		if e.Kind == "suppressed" && strings.HasPrefix(e.Detail, "fire: silence ") {
			found = true
		}
	}
	if !found {
		t.Fatalf("timeline = %+v, want a leg-labelled 'fire: silence ...' event", incs[0].Timeline)
	}

	// legDeliveredStatus must treat this leg as delivered, so a fresh engine
	// (simulating a restart) does not try to resurrect and redeliver it.
	fireDelivered, _ := legDeliveredStatus(incs[0])
	if !fireDelivered {
		t.Fatal("legDeliveredStatus must count a leg-labelled suppressed event as delivered")
	}
}

func TestEngineDeliversAfterSilenceEnded(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0)) // well before the alert fires at 1000

	store := newTestSilenceStore(t)
	sil, err := store.Create(core.Silence{Matchers: []core.Matcher{{Rule: "cpu*"}}, Start: 0, End: ef.now.Unix() + 100, Author: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	ef.engine.SetSilences(store, func(id string) (string, []string) { return "box1", []string{"web"} })

	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", []string{"web"}, ev)
	ef.waitIdle()
	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none while silenced", ef.delivered)
	}

	// The silence ends (expired), but the alert is still firing.
	if err := store.Expire(sil.ID, ef.now.Unix()); err != nil {
		t.Fatal(err)
	}
	ef.now = ef.now.Add(time.Minute)
	ef.engine.TickSilences(ef.now, []string{"n1"})
	ef.waitIdle()

	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered = %+v, want exactly one delivery once the silence ended", ef.delivered)
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || incs[0].State != "firing" {
		t.Fatalf("incidents = %+v, want firing again (delivered)", incs)
	}
	found := false
	for _, e := range incs[0].Timeline {
		if e.Kind == "delivered" && strings.Contains(e.Detail, "delivered after silence ended") {
			found = true
		}
	}
	if !found {
		t.Fatalf("timeline = %+v, want a 'delivered after silence ended' event", incs[0].Timeline)
	}
}

// TestTryDeliverUnsilencedSkipsStaleFireWhenRecoverAlreadyApplied is the
// review round-1 item 3 regression test. The race it guards against: a
// deliverUnsilenced SCAN (at some earlier tick) sees an incident suppressed
// and unresolved and enqueues a re-check job for it; before that job's turn
// in its lane actually comes up, a RECOVER for the very same key arrives and
// is fully Applied. Without moving the delivery DECISION into the job
// itself (and re-reading fresh state there, as tryDeliverUnsilenced now
// does), a scan-time decision would still think it's suppressed and
// unresolved by the time it runs, and deliver a stale "fire" AFTER the
// recover already went out.
//
// This is exercised by calling tryDeliverUnsilenced directly, standing in
// for "the re-check job finally getting its turn in the lane": the actual
// wall-clock race between a background dispatcher goroutine and this
// test's own Submit call is not itself deterministic (nor should a test's
// pass/fail depend on winning it) -- what IS deterministic, and what this
// pins, is that whenever that job DOES run, it must see the incident's
// CURRENT state, not a stale snapshot.
func TestTryDeliverUnsilencedSkipsStaleFireWhenRecoverAlreadyApplied(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))

	store := newTestSilenceStore(t)
	sil, err := store.Create(core.Silence{Matchers: []core.Matcher{{Rule: "cpu*"}}, Start: 0, End: ef.now.Unix() + 100, Author: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	ef.engine.SetSilences(store, func(id string) (string, []string) { return "box1", []string{"web"} })

	fire := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", []string{"web"}, fire)
	ef.waitIdle()
	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none while silenced", ef.delivered)
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %+v, want 1", incs)
	}
	id := incs[0].ID

	// The silence ends, and (in the real race) a scan would have enqueued a
	// re-check job for id here, while it is still suppressed-and-unresolved.
	if err := store.Expire(sil.ID, ef.now.Unix()); err != nil {
		t.Fatal(err)
	}

	// A recover for the same key arrives and is fully processed BEFORE the
	// re-check job's turn comes up -- delivered normally, resolving the
	// incident.
	rec := AlertEvent{Time: 1010, Key: "cpu", Title: "cpu back to normal", Severity: "warning", Kind: "recover", Source: "anomaly", RoutedToMaster: true, FiredAt: 1010}
	ef.engine.HandleChildAlert("n1", "box1", []string{"web"}, rec)
	ef.waitIdle()
	if ef.deliveredCount() != 1 || ef.lastDelivered().Kind != "recover" {
		t.Fatalf("delivered = %+v, want exactly the recover so far", ef.delivered)
	}

	// Now the re-check job finally runs: it must re-read the incident fresh
	// (already resolved) and be a silent no-op -- never a stale fire.
	ef.engine.tryDeliverUnsilenced(id)
	ef.waitIdle()

	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered after the stale re-check = %+v, want still just the recover (no stale fire)", ef.delivered)
	}
	if inc, ok := ef.incidents.Get(id); !ok || inc.State != "resolved" {
		t.Fatalf("incident = %+v, ok=%v, want state resolved (not reopened by a stale fire)", inc, ok)
	}
}

func TestEnginePushesSilencesOnConnectAndOnChange(t *testing.T) {
	ef := newEngineFixture(t)
	store := newTestSilenceStore(t)
	ef.engine.SetSilences(store, func(id string) (string, []string) { return id, []string{"web"} })
	ef.connect("n1")

	ef.engine.PushSilencesNow("n1", ef.now)
	if got := len(ef.framesFor("n1", "silences")); got != 1 {
		t.Fatalf("silences frames after connect-push = %d, want 1", got)
	}

	if _, err := store.Create(core.Silence{Matchers: []core.Matcher{{Tag: "web"}}, Start: 0, End: ef.now.Unix() + 1000, Author: "cli"}); err != nil {
		t.Fatal(err)
	}
	ef.engine.PushSilencesToAll(ef.now, []string{"n1"})
	frames := ef.framesFor("n1", "silences")
	if len(frames) != 2 {
		t.Fatalf("silences frames after change-push = %d, want 2", len(frames))
	}
	var data silencesFrameData
	if err := json.Unmarshal(frames[1].Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Silences) != 1 {
		t.Fatalf("pushed silences = %+v, want the one just created", data.Silences)
	}
}

func TestEngineTickSilencesPrunesLongExpired(t *testing.T) {
	ef := newEngineFixture(t)
	store := newTestSilenceStore(t)
	ef.engine.SetSilences(store, nil)
	weekAgo := ef.now.Unix() - int64(silenceExpiredPruneAfter/time.Second) - 10
	if _, err := store.Create(core.Silence{Matchers: []core.Matcher{{Tag: "old"}}, Start: weekAgo - 100, End: weekAgo}); err != nil {
		t.Fatal(err)
	}
	ef.engine.TickSilences(ef.now, nil)
	if got := store.List(); len(got) != 0 {
		t.Fatalf("silences after TickSilences = %+v, want the long-expired one pruned", got)
	}
}

// --- review round 1, item 1: multi-matcher leak, end to end (web1/db1) -----

// TestSilencesForNodeWeb1DB1MultiMatcherLeak is the review round-1 item 1
// regression test, end to end: a Silence with two OR'd matchers -- "silence
// everything on db1" and "silence disk* alerts everywhere" -- must never let
// web1 suppress an unrelated (mem) alert just because the db1-only matcher
// rode along in the same OR list. web1 must not suppress; db1 must.
func TestSilencesForNodeWeb1DB1MultiMatcherLeak(t *testing.T) {
	store := newTestSilenceStore(t)
	matchers := []core.Matcher{{Node: "db1"}, {Rule: "disk*"}}
	if _, err := store.Create(core.Silence{Matchers: matchers, Start: 0, End: 5000, Author: "cli"}); err != nil {
		t.Fatal(err)
	}

	// Master side (fix a): web1 must be pushed ONLY the matcher that could
	// apply to it (the global disk* one); db1 gets both.
	web1Pushed := store.silencesForNode(1000, "id-web1", "web1", nil)
	if len(web1Pushed) != 1 || len(web1Pushed[0].Matchers) != 1 || web1Pushed[0].Matchers[0].Node != "" {
		t.Fatalf("silencesForNode(web1) = %+v, want only the Rule:disk* matcher (not Node:db1)", web1Pushed)
	}
	db1Pushed := store.silencesForNode(1000, "id-db1", "db1", nil)
	if len(db1Pushed) != 1 || len(db1Pushed[0].Matchers) != 2 {
		t.Fatalf("silencesForNode(db1) = %+v, want both matchers (Node:db1 applies, Rule:disk* applies to every node)", db1Pushed)
	}

	// Child side: feed each node's own filtered push into its own
	// pushedSilences and check a MEM alert (unrelated to disk*).
	web1 := newPushedSilences(filepath.Join(t.TempDir(), "silences.json"))
	if err := web1.Set(web1Pushed); err != nil {
		t.Fatal(err)
	}
	db1 := newPushedSilences(filepath.Join(t.TempDir(), "silences.json"))
	if err := db1.Set(db1Pushed); err != nil {
		t.Fatal(err)
	}

	if _, ok := web1.Suppressed(1000, "mem_pct", "warning"); ok {
		t.Fatal("web1 must NOT suppress its mem alert: the db1-only matcher never reached it, and disk* doesn't match mem")
	}
	if reason, ok := db1.Suppressed(1000, "mem_pct", "warning"); !ok || reason != fmt.Sprintf("silence %s by cli", db1Pushed[0].ID) {
		t.Fatalf("db1 Suppressed = %q, %v, want suppressed (silence everything on db1)", reason, ok)
	}
}
