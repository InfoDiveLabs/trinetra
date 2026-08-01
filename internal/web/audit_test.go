package web

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readAuditRecords reads back every line of <stateDir>/audit.jsonl as
// AuditRecords, for tests to assert against. A missing file yields an empty
// (not error) slice, mirroring the log-reading conventions elsewhere in this
// codebase (e.g. AlertLog.AlertEventsSince).
func readAuditRecords(t *testing.T, stateDir string) []AuditRecord {
	t.Helper()
	f, err := os.Open(auditLogPath(stateDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open audit log: %v", err)
	}
	defer f.Close()
	var out []AuditRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("decode audit line %q: %v", sc.Text(), err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan audit log: %v", err)
	}
	return out
}

// TestAppendAuditWritesExpectedFields pins the core contract: an appended
// record round-trips with its Action/Key/Old/New intact and a non-zero
// Time auto-filled.
func TestAppendAuditWritesExpectedFields(t *testing.T) {
	dir := t.TempDir()
	if err := appendAudit(dir, AuditRecord{User: "root", Action: "config.set", Key: "thresholds.disk_pct", Old: "90", New: "95"}); err != nil {
		t.Fatalf("appendAudit: %v", err)
	}
	recs := readAuditRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.User != "root" || r.Action != "config.set" || r.Key != "thresholds.disk_pct" || r.Old != "90" || r.New != "95" {
		t.Errorf("record = %+v, want the fields passed in", r)
	}
	if r.Time == 0 {
		t.Error("Time was not auto-filled")
	}
}

// TestAppendAuditAppendsAcrossCalls pins that repeated appends accumulate as
// separate JSONL lines rather than clobbering the file.
func TestAppendAuditAppendsAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := appendAudit(dir, AuditRecord{Action: "test", Key: string(rune('a' + i))}); err != nil {
			t.Fatalf("appendAudit #%d: %v", i, err)
		}
	}
	recs := readAuditRecords(t, dir)
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3", len(recs))
	}
	for i, r := range recs {
		if r.Key != string(rune('a'+i)) {
			t.Errorf("record %d key = %q, want %q", i, r.Key, string(rune('a'+i)))
		}
	}
}

// TestAppendAuditEmptyStateDirIsNoop pins that an empty StateDir (some
// minimal test Deps) is a silent no-op, never an error — the audit trail
// must never fail the mutation it's describing just because no state
// directory was configured.
func TestAppendAuditEmptyStateDirIsNoop(t *testing.T) {
	if err := appendAudit("", AuditRecord{Action: "test"}); err != nil {
		t.Fatalf("appendAudit with empty stateDir: %v", err)
	}
}

// TestAppendAuditCreatesStateDir pins that a not-yet-existing StateDir (a
// fresh install with no config/channel edits yet) is created rather than
// erroring.
func TestAppendAuditCreatesStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	if err := appendAudit(dir, AuditRecord{Action: "test"}); err != nil {
		t.Fatalf("appendAudit: %v", err)
	}
	if _, err := os.Stat(auditLogPath(dir)); err != nil {
		t.Fatalf("audit log not created: %v", err)
	}
}
