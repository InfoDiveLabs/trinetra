package trinetra

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditLogAppendAndRecentNewestFirst(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet", "audit.jsonl")
	a := newAuditLog(path)

	if err := a.Append("cli", "revoke_node", "n1", "", 1000); err != nil {
		t.Fatal(err)
	}
	if err := a.Append("cli", "rename_node", "n1", "new-name", 1010); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("audit.jsonl perm = %o, want 0600", perm)
	}

	entries, err := a.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Action != "rename_node" || entries[1].Action != "revoke_node" {
		t.Fatalf("entries = %+v, want newest first", entries)
	}

	limited, err := a.Recent(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Action != "rename_node" {
		t.Fatalf("limited = %+v", limited)
	}
}

func TestAuditLogEmptyActorDefaultsToUnknown(t *testing.T) {
	dir := t.TempDir()
	a := newAuditLog(filepath.Join(dir, "audit.jsonl"))
	if err := a.Append("", "create_token", "t1", "", 1000); err != nil {
		t.Fatal(err)
	}
	entries, err := a.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Actor != "unknown" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestAuditLogMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	a := newAuditLog(filepath.Join(dir, "does-not-exist", "audit.jsonl"))
	entries, err := a.Recent(0)
	if err != nil || entries != nil {
		t.Fatalf("entries = %+v, err = %v", entries, err)
	}
}

func TestAuditLogNilIsANoOp(t *testing.T) {
	var a *auditLog
	if err := a.Append("cli", "revoke_node", "n1", "", 1000); err != nil {
		t.Fatalf("nil auditLog.Append err = %v", err)
	}
	entries, err := a.Recent(0)
	if err != nil || entries != nil {
		t.Fatalf("nil auditLog.Recent = %+v, err = %v", entries, err)
	}
}
