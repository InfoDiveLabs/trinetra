package trinetra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// --- master-side: save validation ----------------------------------------

func TestManagedFragmentSaveRejectsKeyOutsideAllowlist(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Save(core.ManagedFragment{Values: map[string]string{"fleet.role": "master"}}, "cli")
	if err == nil || !strings.Contains(err.Error(), "fleet.role") {
		t.Fatalf("Save() err = %v, want it to name the disallowed key", err)
	}
}

func TestManagedFragmentSaveRejectsBadValue(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Save(core.ManagedFragment{Values: map[string]string{"thresholds.cpu_pct": "not-a-number"}}, "cli")
	if err == nil {
		t.Fatal("Save() with a bad float value must fail")
	}
	if len(s.List()) != 0 {
		t.Fatal("a rejected save must not be stored")
	}
}

func TestManagedFragmentSaveAssignsIDVersionAndAuthor(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Save(core.ManagedFragment{Values: map[string]string{"thresholds.cpu_pct": "88"}}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" || saved.Version != 1 || saved.Author != "alice" {
		t.Fatalf("saved = %+v, want a fresh id, version 1, author alice", saved)
	}
	// A second save (create) bumps the store's generation again.
	saved2, err := s.Save(core.ManagedFragment{Tag: "web", Values: map[string]string{"thresholds.mem_pct": "70"}}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if saved2.Version != 2 || saved2.ID == saved.ID {
		t.Fatalf("saved2 = %+v, want version 2 and a distinct id", saved2)
	}
	// Updating the FIRST fragment by ID bumps the version again.
	updated, err := s.Save(core.ManagedFragment{ID: saved.ID, Values: map[string]string{"thresholds.cpu_pct": "91"}}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 3 || updated.ID != saved.ID {
		t.Fatalf("updated = %+v, want version 3 keeping the same id", updated)
	}
	if len(s.List()) != 2 {
		t.Fatalf("List() = %d fragments, want 2 (one updated, one untouched)", len(s.List()))
	}
}

func TestManagedFragmentSaveUnknownIDRejected(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(core.ManagedFragment{ID: "doesnotexist", Values: map[string]string{"baseline_sigma": "3"}}, "cli"); err == nil {
		t.Fatal("Save() with an unknown id must fail")
	}
}

func TestManagedFragmentDeleteBumpsVersionAndRemoves(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Save(core.ManagedFragment{Values: map[string]string{"baseline_sigma": "3"}}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(saved.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("Delete must remove the fragment")
	}
	if s.Version() != 2 {
		t.Fatalf("Version() = %d, want 2 (save bumped to 1, delete to 2)", s.Version())
	}
	if err := s.Delete(saved.ID); err == nil {
		t.Fatal("deleting an already-deleted fragment must fail")
	}
}

// TestManagedFragmentStorePersists round-trips through disk (atomic write +
// reload), mirroring loadSilenceStore's own persistence test shape.
func TestManagedFragmentStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed.json")
	s, err := loadManagedFragmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(core.ManagedFragment{Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "80"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadManagedFragmentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.List()) != 1 || reloaded.Version() != 1 {
		t.Fatalf("reloaded store = %+v version %d, want 1 fragment at version 1", reloaded.List(), reloaded.Version())
	}
}

// --- master-side: merge order and conflicts -------------------------------

func TestManagedFragmentDesiredMergeOrderTagAlphabeticalLaterWins(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	// "" (all nodes): cpu=50, mem=60
	if _, err := s.Save(core.ManagedFragment{Values: map[string]string{"thresholds.cpu_pct": "50", "thresholds.mem_pct": "60"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	// tag "app": cpu=70 (should beat "" but lose to "web", since web > app alphabetically)
	if _, err := s.Save(core.ManagedFragment{Tag: "app", Values: map[string]string{"thresholds.cpu_pct": "70"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	// tag "web": cpu=90 (applied last -- alphabetically after "app" -- so wins)
	if _, err := s.Save(core.ManagedFragment{Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "90"}}, "cli"); err != nil {
		t.Fatal(err)
	}

	values, keyFragment, conflicts := s.Desired([]string{"web", "app"})
	if values["thresholds.cpu_pct"] != "90" {
		t.Fatalf("cpu_pct = %q, want 90 (tag web applied last, alphabetically after app)", values["thresholds.cpu_pct"])
	}
	if values["thresholds.mem_pct"] != "60" {
		t.Fatalf("mem_pct = %q, want 60 (only the all-nodes fragment sets it)", values["thresholds.mem_pct"])
	}
	webID, _ := s.ByTag("web")
	if keyFragment["thresholds.cpu_pct"] != webID.ID {
		t.Fatalf("keyFragment[cpu_pct] = %q, want the web fragment %q", keyFragment["thresholds.cpu_pct"], webID.ID)
	}
	if len(conflicts) != 1 || conflicts[0].Key != "thresholds.cpu_pct" || len(conflicts[0].Fragments) != 3 {
		t.Fatalf("conflicts = %+v, want one conflict on cpu_pct naming all 3 contributing fragments", conflicts)
	}

	// A node with only the "app" tag never sees "web"'s value.
	values2, _, conflicts2 := s.Desired([]string{"app"})
	if values2["thresholds.cpu_pct"] != "70" {
		t.Fatalf("cpu_pct (app only) = %q, want 70", values2["thresholds.cpu_pct"])
	}
	if len(conflicts2) != 1 {
		t.Fatalf("conflicts (app only) = %+v, want one conflict ('' and 'app' both set cpu_pct)", conflicts2)
	}

	// A node with no tags at all only ever sees the all-nodes fragment, no
	// conflicts.
	values3, _, conflicts3 := s.Desired(nil)
	if values3["thresholds.cpu_pct"] != "50" || len(conflicts3) != 0 {
		t.Fatalf("cpu_pct (no tags) = %q conflicts=%+v, want 50 and no conflicts", values3["thresholds.cpu_pct"], conflicts3)
	}
}

// --- master-side: push on change / connect / periodic ---------------------

// pushCapture is a tiny fleet.Frame push/connected double for managedPusher
// tests: every push overwrites that node's last frame (mirroring the "push
// the CURRENT desired set" semantics -- there is no queue).
type pushCapture struct {
	frames    map[string]fleet.Frame
	connected map[string]bool
}

func newPushCapture() *pushCapture {
	return &pushCapture{frames: map[string]fleet.Frame{}, connected: map[string]bool{}}
}
func (p *pushCapture) push(id string, f fleet.Frame) bool {
	p.frames[id] = f
	return true
}
func (p *pushCapture) isConnected(id string) bool { return p.connected[id] }

func decodeManagedFrame(t *testing.T, f fleet.Frame) managedConfigFrameData {
	t.Helper()
	if f.Type != "managed_config" {
		t.Fatalf("frame type = %q, want managed_config", f.Type)
	}
	var p managedConfigFrameData
	if err := json.Unmarshal(f.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManagedPusherPushOneBuildsFilteredFrame(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.Save(core.ManagedFragment{Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "90"}}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	pc := newPushCapture()
	pusher := newManagedPusher(pc.push, pc.isConnected, s, func(id string) []string {
		if id == "n1" {
			return []string{"web"}
		}
		return nil
	})
	if !pusher.PushOne("n1") {
		t.Fatal("PushOne must report success")
	}
	got := decodeManagedFrame(t, pc.frames["n1"])
	if got.Version != s.Version() || got.Values["thresholds.cpu_pct"] != "90" || got.Fragments["thresholds.cpu_pct"] != saved.ID {
		t.Fatalf("pushed frame = %+v, want version %d, cpu_pct=90 from fragment %s", got, s.Version(), saved.ID)
	}
	// A node without the "web" tag gets an empty desired set.
	pusher.PushOne("n2")
	got2 := decodeManagedFrame(t, pc.frames["n2"])
	if len(got2.Values) != 0 {
		t.Fatalf("n2's pushed values = %+v, want empty (no matching tag)", got2.Values)
	}
}

func TestManagedPusherPushToAllSkipsDisconnected(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(core.ManagedFragment{Values: map[string]string{"baseline_sigma": "3"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	pc := newPushCapture()
	pc.connected["n1"] = true
	pusher := newManagedPusher(pc.push, pc.isConnected, s, func(string) []string { return nil })
	pusher.PushToAll([]string{"n1", "n2"})
	if _, ok := pc.frames["n1"]; !ok {
		t.Fatal("connected node n1 must be pushed")
	}
	if _, ok := pc.frames["n2"]; ok {
		t.Fatal("disconnected node n2 must not be pushed")
	}
}

func TestManagedPusherTickManagedSelfGates(t *testing.T) {
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(core.ManagedFragment{Values: map[string]string{"baseline_sigma": "3"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	pc := newPushCapture()
	pc.connected["n1"] = true
	pusher := newManagedPusher(pc.push, pc.isConnected, s, func(string) []string { return nil })

	now := time.Unix(1000, 0)
	pusher.TickManaged(now, []string{"n1"})
	if _, ok := pc.frames["n1"]; !ok {
		t.Fatal("the first TickManaged must push unconditionally")
	}
	delete(pc.frames, "n1")
	pusher.TickManaged(now.Add(time.Minute), []string{"n1"})
	if _, ok := pc.frames["n1"]; ok {
		t.Fatal("TickManaged must not push again before managedPushInterval elapses")
	}
	pusher.TickManaged(now.Add(managedPushInterval+time.Second), []string{"n1"})
	if _, ok := pc.frames["n1"]; !ok {
		t.Fatal("TickManaged must push again once managedPushInterval has elapsed")
	}
}

// --- fleetAPIImpl: SaveManaged/DeleteManaged push on change, ManagedStatus -

// managedTestMasterState extends newTestMasterState (fleet_provider_test.go)
// with a managed-config store/pusher wired to a pushCapture, so
// fleetAPIImpl.SaveManaged/DeleteManaged/ManagedStatus can be exercised
// without a full startMaster/real network Hub.
func managedTestMasterState(t *testing.T) (*masterState, *pushCapture) {
	t.Helper()
	m := newTestMasterState(t)
	s, err := loadManagedFragmentStore(filepath.Join(t.TempDir(), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.managed = s
	pc := newPushCapture()
	m.managedPush = newManagedPusher(pc.push, pc.isConnected, s, func(id string) []string {
		n, ok := m.reg.Get(id)
		if !ok {
			return nil
		}
		return n.Tags
	})
	return m, pc
}

func TestFleetAPISaveManagedPushesOnChangeAndAudits(t *testing.T) {
	m, pc := managedTestMasterState(t)
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: nodeID, Name: "web1", Tags: []string{"web"}, Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	pc.connected[nodeID] = true
	api := fleetAPIFor(m)

	saved, err := api.SaveManaged(core.ManagedFragment{Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "85"}}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" {
		t.Fatal("SaveManaged must assign an id")
	}
	got := decodeManagedFrame(t, pc.frames[nodeID])
	if got.Values["thresholds.cpu_pct"] != "85" {
		t.Fatalf("push-on-change frame = %+v, want cpu_pct=85", got)
	}
	entries, err := api.Audit(10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "fleet.managed.save" && e.Actor == "alice" && e.Target == saved.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit log = %+v, want a fleet.managed.save entry by alice", entries)
	}

	// DeleteManaged also pushes (an now-empty desired set) and audits.
	delete(pc.frames, nodeID)
	if err := api.DeleteManaged(saved.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	got2 := decodeManagedFrame(t, pc.frames[nodeID])
	if len(got2.Values) != 0 {
		t.Fatalf("push-on-delete frame = %+v, want an empty desired set", got2)
	}
	entries2, _ := api.Audit(10)
	found = false
	for _, e := range entries2 {
		if e.Action == "fleet.managed.delete" && e.Target == saved.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a fleet.managed.delete audit entry")
	}
}

func TestFleetAPISaveManagedRejectsBadKeyNoAuditNoPush(t *testing.T) {
	m, pc := managedTestMasterState(t)
	api := fleetAPIFor(m)
	if _, err := api.SaveManaged(core.ManagedFragment{Values: map[string]string{"quiet_hours": "not-valid"}}, "alice"); err == nil {
		t.Fatal("SaveManaged must reject an invalid value")
	}
	if len(pc.frames) != 0 {
		t.Fatal("a rejected SaveManaged must never push")
	}
	entries, _ := api.Audit(10)
	if len(entries) != 0 {
		t.Fatalf("a rejected SaveManaged must never audit, got %+v", entries)
	}
}

// TestFleetAPIManagedStatusComputesDriftAndConflicts drives ManagedStatus
// against a node whose LiveUpdate.Managed reports stale values for one
// desired key, and never reported another (never connected before) -- both
// must appear in Drift; a second, fully-in-sync node must report no drift.
func TestFleetAPIManagedStatusComputesDriftAndConflicts(t *testing.T) {
	m, _ := managedTestMasterState(t)
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: nodeID, Name: "web1", Tags: []string{"web"}, Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	inSyncID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: inSyncID, Name: "web2", Tags: []string{"web"}, Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	api := fleetAPIFor(m)
	if _, err := api.SaveManaged(core.ManagedFragment{Tag: "web", Values: map[string]string{"thresholds.cpu_pct": "85", "baseline_sigma": "3"}}, "cli"); err != nil {
		t.Fatal(err)
	}

	// nodeID last reported cpu_pct=80 (stale) and never reported
	// baseline_sigma at all -- both must show as drift.
	if err := m.sink.Live(nodeID, fleet.LiveUpdate{
		Managed: &fleet.ManagedReport{Version: 1, Applied: true, Values: map[string]string{"thresholds.cpu_pct": "80"}},
	}); err != nil {
		t.Fatal(err)
	}
	// inSyncID matches the desired set exactly.
	if err := m.sink.Live(inSyncID, fleet.LiveUpdate{
		Managed: &fleet.ManagedReport{Version: 1, Applied: true, Values: map[string]string{"thresholds.cpu_pct": "85", "baseline_sigma": "3"}},
	}); err != nil {
		t.Fatal(err)
	}

	statuses, err := api.ManagedStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("ManagedStatus() = %+v, want 2 entries", statuses)
	}
	byNode := map[string]core.ManagedStatus{}
	for _, s := range statuses {
		byNode[s.Node] = s
	}
	drifted := byNode[nodeID]
	sort.Strings(drifted.Drift)
	if len(drifted.Drift) != 2 || drifted.Drift[0] != "baseline_sigma" || drifted.Drift[1] != "thresholds.cpu_pct" {
		t.Fatalf("drifted node's Drift = %v, want both keys", drifted.Drift)
	}
	if !drifted.Applied {
		t.Fatalf("drifted node's Applied should reflect its own report (true), got %+v", drifted)
	}
	inSync := byNode[inSyncID]
	if len(inSync.Drift) != 0 {
		t.Fatalf("in-sync node's Drift = %v, want none", inSync.Drift)
	}
}

// --- child-side: apply, live-apply-no-restart, invalid rejects whole ------

// managedApplyAPI is a minimal core.API double for managedChild.Apply tests:
// it embeds fleetCLIFakeAPI (fleet_cmd_test.go, a full core.API stub) for
// every method except ApplyConfig, which it overrides to actually mimic the
// real reload path: it stores whatever config was applied so Get(key) after
// Apply proves the value took effect live, with no restart, and can be
// primed to fail (an invalid managed-config value must never even reach
// here -- see the "invalid value rejects the whole fragment" tests -- but a
// downstream apply failure, e.g. a full reload error, must still surface).
type managedApplyAPI struct {
	fleetCLIFakeAPI
	applied *config.Config
	err     error
}

func (a *managedApplyAPI) ApplyConfig(c *config.Config) error {
	if a.err != nil {
		return a.err
	}
	a.applied = c
	return nil
}

func TestManagedChildAppliesLiveNoRestart(t *testing.T) {
	cfg := config.Default()
	self := &managedApplyAPI{}
	// getCfg mirrors the real daemon's reload semantics (fleetDeps.getCfg
	// reads whatever ApplyConfig's reload closure last swapped in): once
	// self.applied is set, subsequent reads see the NEW config, not the
	// original -- this is what lets Report()/`config get` observe the
	// applied value with no restart.
	getCfg := func() *config.Config {
		if self.applied != nil {
			return self.applied
		}
		return cfg
	}
	mc := newManagedChild(filepath.Join(t.TempDir(), "managed.json"), getCfg, self, nil)

	mc.Apply(managedConfigFrameData{
		Version:   1,
		Values:    map[string]string{"thresholds.cpu_pct": "77", "quiet_hours": "23-8"},
		Fragments: map[string]string{"thresholds.cpu_pct": "frag1", "quiet_hours": "frag1"},
	})

	if self.applied == nil {
		t.Fatal("ApplyConfig must be called (the existing live-apply/reload path)")
	}
	if v, ok := self.applied.Get("thresholds.cpu_pct"); !ok || v != "77" {
		t.Fatalf("applied config get(thresholds.cpu_pct) = %q, %v, want 77", v, ok)
	}
	report := mc.Report()
	if report == nil || !report.Applied || report.Version != 1 || report.Error != "" {
		t.Fatalf("Report() = %+v, want Version 1, Applied true, no error", report)
	}
	if report.Values["thresholds.cpu_pct"] != "77" {
		t.Fatalf("Report().Values = %+v, want cpu_pct=77 (read from the now-live config)", report.Values)
	}
	// Persisted sidecar round-trips version/values/fragments.
	b, err := os.ReadFile(filepath.Join(filepath.Dir(mc.path), "managed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk managedChildFileV1
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Version != 1 || onDisk.Values["quiet_hours"] != "23-8" || onDisk.Fragments["thresholds.cpu_pct"] != "frag1" {
		t.Fatalf("on-disk sidecar = %+v, want version 1, quiet_hours=23-8, fragment attribution", onDisk)
	}
}

func TestManagedChildInvalidValueRejectsWholeFragmentKeepsOldConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Thresholds.CPUPct = 90 // the "old" value that must survive
	self := &managedApplyAPI{}
	mc := newManagedChild(filepath.Join(t.TempDir(), "managed.json"), func() *config.Config { return cfg }, self, nil)

	// A syntactically valid first key, then an invalid second key (bad
	// float): the WHOLE fragment must be rejected, not just the bad key.
	mc.Apply(managedConfigFrameData{
		Version: 5,
		Values:  map[string]string{"thresholds.mem_pct": "60", "thresholds.cpu_pct": "not-a-number"},
	})

	if self.applied != nil {
		t.Fatal("ApplyConfig must never be called when any value is invalid")
	}
	if cfg.Thresholds.CPUPct != 90 {
		t.Fatalf("live config CPUPct = %v, want unchanged 90 (old config kept)", cfg.Thresholds.CPUPct)
	}
	report := mc.Report()
	if report == nil || report.Applied || report.Error == "" {
		t.Fatalf("Report() = %+v, want Applied false with a non-empty Error", report)
	}
	if report.Version != 0 {
		t.Fatalf("Report().Version = %d, want 0 (never successfully applied anything yet)", report.Version)
	}
}

func TestManagedChildEmptyValuesClearsSidecarKeepsLocalConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed.json")
	cfg := config.Default()
	self := &managedApplyAPI{}
	mc := newManagedChild(path, func() *config.Config { return cfg }, self, nil)

	mc.Apply(managedConfigFrameData{Version: 1, Values: map[string]string{"thresholds.cpu_pct": "77"}, Fragments: map[string]string{"thresholds.cpu_pct": "frag1"}})
	if _, err := os.Stat(path); err != nil {
		t.Fatal("sidecar must exist after a successful apply")
	}
	appliedCPU := self.applied.Thresholds.CPUPct

	// All fragments deleted on the master: an empty Values frame.
	mc.Apply(managedConfigFrameData{Version: 2, Values: map[string]string{}, Fragments: map[string]string{}})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("sidecar must be removed once every fragment is deleted, stat err = %v", err)
	}
	if appliedCPU != 77 {
		t.Fatalf("sanity: the earlier apply should have set CPUPct=77, got %v", appliedCPU)
	}
	// Local values are left exactly as they were (task-8 ruling): nothing
	// un-applies them, ApplyConfig is not called again with a reverted value.
	if self.applied.Thresholds.CPUPct != 77 {
		t.Fatalf("CPUPct after clearing = %v, want unchanged 77 (local values stay at their last managed values)", self.applied.Thresholds.CPUPct)
	}
	report := mc.Report()
	if report == nil || !report.Applied || report.Error != "" {
		t.Fatalf("Report() after clear = %+v, want Applied true, no error", report)
	}
	if fid, managed := ManagedFragmentFor(dir, "thresholds.cpu_pct"); managed {
		t.Fatalf("ManagedFragmentFor after clear = (%q, managed), want not managed any more", fid)
	}
}

// TestManagedChildReportNilBeforeAnyFrameReceived pins "an old master (no
// frames) leaves the child unaffected": a managedChild that has never had
// Apply called on it (no "managed_config" frame ever arrived) reports nil,
// so LiveUpdate.Managed is omitted entirely, exactly as if task 8 did not
// exist for this child.
func TestManagedChildReportNilBeforeAnyFrameReceived(t *testing.T) {
	cfg := config.Default()
	mc := newManagedChild(filepath.Join(t.TempDir(), "managed.json"), func() *config.Config { return cfg }, &managedApplyAPI{}, nil)
	if r := mc.Report(); r != nil {
		t.Fatalf("Report() = %+v, want nil before any managed_config frame ever arrives", r)
	}
	// applyManagedConfigFrame must ignore any OTHER frame type too.
	applyManagedConfigFrame(mc, fleet.Frame{Type: "lease", Data: json.RawMessage(`{"until":123}`)})
	if r := mc.Report(); r != nil {
		t.Fatalf("Report() = %+v, want still nil after an unrelated frame type", r)
	}
}

// TestManagedFragmentForReadsRestoredSidecar exercises the exported helper
// `trinetra config set`'s cmdConfig calls: a fresh managedChild loaded from
// a sidecar written by a PREVIOUS process must still name the right
// fragment for a managed key, and report not-managed for anything else.
func TestManagedFragmentForReadsRestoredSidecar(t *testing.T) {
	dir := t.TempDir()
	path := managedChildPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(managedChildFileV1{
		Version:   3,
		Values:    map[string]string{"thresholds.cpu_pct": "80"},
		Fragments: map[string]string{"thresholds.cpu_pct": "abc123def456"},
		AppliedAt: 1000,
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if id, managed := ManagedFragmentFor(dir, "thresholds.cpu_pct"); !managed || id != "abc123def456" {
		t.Fatalf("ManagedFragmentFor = (%q, %v), want (abc123def456, true)", id, managed)
	}
	if _, managed := ManagedFragmentFor(dir, "thresholds.mem_pct"); managed {
		t.Fatal("a key never in the sidecar's Values must not be reported managed")
	}
	if _, managed := ManagedFragmentFor(dir, "thresholds.cpu_pct"); !managed {
		t.Fatal("sanity re-check failed")
	}

	// loadManagedChild restores the same state for the live daemon path.
	cfg := config.Default()
	mc := loadManagedChild(path, func() *config.Config { return cfg }, &managedApplyAPI{}, nil)
	if got := mc.FragmentsSnapshot()["thresholds.cpu_pct"]; got != "abc123def456" {
		t.Fatalf("FragmentsSnapshot()[cpu_pct] = %q, want abc123def456", got)
	}
	report := mc.Report()
	if report == nil || report.Version != 3 || !report.Applied {
		t.Fatalf("Report() after restore = %+v, want Version 3, Applied true", report)
	}
}
