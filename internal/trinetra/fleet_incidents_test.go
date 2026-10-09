package trinetra

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func TestIncidentStoreFireOpensThenRecoverCloses(t *testing.T) {
	dir := t.TempDir()
	s, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{NodeID: "n1", NodeName: "box1"}
	fire := Alert{Key: "cpu", Title: "cpu high", Severity: SevWarning, Kind: "fire", Time: 1000}
	inc, err := s.Apply(incidentApply{src: src, alert: fire, firedAt: 1000, now: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != "firing" || inc.GroupKey != "n1:cpu" || len(inc.Alerts) != 1 {
		t.Fatalf("fire result = %+v", inc)
	}
	if len(inc.ID) != 12 {
		t.Fatalf("id = %q, want 12 hex chars", inc.ID)
	}

	recov := Alert{Key: "cpu", Title: "cpu back to normal", Severity: SevWarning, Kind: "recover", Time: 1050}
	inc2, err := s.Apply(incidentApply{src: src, alert: recov, firedAt: 1050, now: 1050})
	if err != nil {
		t.Fatal(err)
	}
	if inc2.ID != inc.ID {
		t.Fatalf("recover opened a NEW incident: %s vs %s", inc2.ID, inc.ID)
	}
	if inc2.State != "resolved" || inc2.Resolved != 1050 || inc2.Alerts[0].ResolvedAt != 1050 {
		t.Fatalf("recover result = %+v", inc2)
	}
	if _, stillOpen := s.OpenForGroupKey("n1:cpu"); stillOpen {
		t.Fatal("group key still marked open after resolve")
	}

	// A later fire for the same (node, key) opens a FRESH incident, not the
	// resolved one.
	inc3, err := s.Apply(incidentApply{src: src, alert: fire, firedAt: 2000, now: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if inc3.ID == inc.ID {
		t.Fatal("a new episode reused the resolved incident's id")
	}
}

func TestIncidentStoreLoadLastLineWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")
	s, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{NodeID: "n1", NodeName: "box1"}
	if _, err := s.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "fire", Time: 1000}, firedAt: 1000, now: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ack("nonexistent", "cli", 1010); err == nil {
		t.Fatal("ack on unknown id should fail")
	}
	incs := s.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %+v", incs)
	}
	id := incs[0].ID
	if _, err := s.Ack(id, "cli", 1010); err != nil {
		t.Fatal(err)
	}

	// Reload: the LAST line for id (the ack) must win over the earlier fire
	// snapshot.
	s2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	inc, ok := s2.Get(id)
	if !ok || inc.State != "acked" || inc.AckedBy != "cli" {
		t.Fatalf("reloaded incident = %+v ok=%v", inc, ok)
	}
	// An acked incident is still "open" for grouping purposes.
	if _, open := s2.OpenForGroupKey("n1:cpu"); !open {
		t.Fatal("acked incident should still be open")
	}
}

func TestIncidentStoreRotatesAt50MB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")
	// Pre-create a file already at the rotation threshold: harmless
	// (non-JSON, silently skipped by load), newline-delimited, reasonably
	// long lines -- fast to scan, unlike one huge token or millions of tiny
	// ones -- but still a real newline-delimited file, like the genuine
	// incidents.jsonl this stands in for.
	line := append(bytes.Repeat([]byte("x"), 4096), '\n')
	buf := bytes.Repeat(line, incidentRotateBytes/len(line)+1)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(incidentApply{src: alertSource{NodeID: "n1"}, alert: Alert{Key: "cpu", Kind: "fire", Time: 1}, firedAt: 1, now: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated file %s.1: %v", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= incidentRotateBytes {
		t.Fatalf("post-rotation file still huge: %d bytes", fi.Size())
	}
}

func TestIncidentStoreFindByAlertKeyAndListFilters(t *testing.T) {
	dir := t.TempDir()
	s, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(incidentApply{src: alertSource{NodeID: "n1"}, alert: Alert{Key: "cpu", Kind: "fire", Time: 1000}, firedAt: 1000, now: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(incidentApply{src: alertSource{NodeID: "n2"}, alert: Alert{Key: "mem", Kind: "fire", Time: 1000}, firedAt: 1000, now: 1001}); err != nil {
		t.Fatal(err)
	}

	inc, ok := s.FindByAlertKey("mem")
	if !ok || inc.GroupKey != "n2:mem" {
		t.Fatalf("FindByAlertKey(mem) = %+v ok=%v", inc, ok)
	}
	if _, ok := s.FindByAlertKey("disk"); ok {
		t.Fatal("FindByAlertKey(disk) should not match anything")
	}

	byNode := s.List(core.IncidentFilter{Node: "n1"}, nil)
	if len(byNode) != 1 || byNode[0].GroupKey != "n1:cpu" {
		t.Fatalf("List(Node=n1) = %+v", byNode)
	}
	byTag := s.List(core.IncidentFilter{Tag: "web"}, func(id string) []string {
		if id == "n2" {
			return []string{"web"}
		}
		return nil
	})
	if len(byTag) != 1 || byTag[0].GroupKey != "n2:mem" {
		t.Fatalf("List(Tag=web) = %+v", byTag)
	}
	limited := s.List(core.IncidentFilter{Limit: 1}, nil)
	if len(limited) != 1 {
		t.Fatalf("List(Limit=1) = %+v", limited)
	}
}

func TestIncidentStoreSeenKeysCoversFireAndRecover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")
	s, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{NodeID: "n1"}
	if _, err := s.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "fire", Time: 1000}, firedAt: 1000, now: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "recover", Time: 1050}, firedAt: 1050, now: 1050}); err != nil {
		t.Fatal(err)
	}

	s2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := s2.seenKeys()
	if _, ok := seen[alertDedupKey{node: "n1", key: "cpu", firedAt: 1000}]; !ok {
		t.Fatal("seenKeys missing the fire")
	}
	if _, ok := seen[alertDedupKey{node: "n1", key: "cpu", firedAt: 1050}]; !ok {
		t.Fatal("seenKeys missing the recover")
	}
}

// TestIncidentStoreAckGuardsResolvedIncidents is a MINOR from the B3 review
// round 1: acking an already-resolved incident must do nothing (return an
// error), not silently flip it back to "acked" while leaving Resolved set.
func TestIncidentStoreAckGuardsResolvedIncidents(t *testing.T) {
	dir := t.TempDir()
	s, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{NodeID: "n1"}
	if _, err := s.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "fire", Time: 1000}, firedAt: 1000, now: 1000}); err != nil {
		t.Fatal(err)
	}
	inc, err := s.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "recover", Time: 1050}, firedAt: 1050, now: 1050})
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != "resolved" {
		t.Fatalf("precondition: incident state = %q, want resolved", inc.State)
	}

	if _, err := s.Ack(inc.ID, "cli", 2000); err == nil {
		t.Fatal("Ack on a resolved incident should fail")
	}

	got, ok := s.Get(inc.ID)
	if !ok || got.State != "resolved" || got.AckedBy != "" {
		t.Fatalf("incident after a refused ack = %+v ok=%v", got, ok)
	}
}

// TestIncidentStoreAppendEventAndMarkDeliveredLocally exercises the two
// primitives fleetAlertEngine.deliverAndReceipt/Submit's alreadySeen branch
// use directly, independent of the engine.
func TestIncidentStoreAppendEventAndMarkDeliveredLocally(t *testing.T) {
	dir := t.TempDir()
	s, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{NodeID: "n1", NodeName: "box1"}
	inc, err := s.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "fire", Time: 1000}, firedAt: 1000, now: 1000})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := s.AppendEvent(inc.ID, core.IncidentEvent{TS: 1010, Kind: "delivered", Detail: "sent via the master's dispatcher", Actor: "system"})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Timeline) != 2 || updated.Timeline[1].Kind != "delivered" || updated.Updated != 1010 {
		t.Fatalf("incident after AppendEvent = %+v", updated)
	}
	if _, err := s.AppendEvent("no-such-id", core.IncidentEvent{TS: 1010, Kind: "delivered"}); err == nil {
		t.Fatal("AppendEvent on an unknown id should fail")
	}

	marked, ok, err := s.MarkDeliveredLocally("n1", "cpu", 1000, 1020)
	if err != nil || !ok || !marked.Alerts[0].DeliveredLocally {
		t.Fatalf("MarkDeliveredLocally = %+v ok=%v err=%v", marked, ok, err)
	}
	foundChildEvent := false
	for _, e := range marked.Timeline {
		if e.Kind == "delivered" && e.Actor == "child" {
			foundChildEvent = true
		}
	}
	if !foundChildEvent {
		t.Fatalf("MarkDeliveredLocally did not append a child-delivered event: %+v", marked.Timeline)
	}

	// No matching alert at all: a no-op, not an error.
	_, ok, err = s.MarkDeliveredLocally("n1", "cpu", 9999, 1030)
	if err != nil || ok {
		t.Fatalf("MarkDeliveredLocally for an unknown (node,key,firedAt) = ok=%v err=%v", ok, err)
	}
}
