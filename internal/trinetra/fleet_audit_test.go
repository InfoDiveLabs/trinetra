package trinetra

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestAuditLogRecentOnLargeFileReturnsNewestBoundedRead: against a 50k-line
// audit log, Recent(50) returns exactly the newest 50 entries, newest first,
// via a small backward read from EOF rather than a forward scan. The bound is
// asserted by wall time instead of instrumenting the file descriptor.
func TestAuditLogRecentOnLargeFileReturnsNewestBoundedRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	const total = 50000
	var buf bytes.Buffer
	for i := 0; i < total; i++ {
		buf.WriteString(fmt.Sprintf(`{"ts":%d,"actor":"cli","action":"revoke_node","target":"n%d"}`, 1000+i, i))
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	a := newAuditLog(path)
	start := time.Now()
	const limit = 50
	entries, err := a.Recent(limit)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != limit {
		t.Fatalf("len(entries) = %d, want %d", len(entries), limit)
	}
	// Newest first: target n49999 (the last line written) comes first,
	// counting down to n49950 (the 50th-from-last).
	for i, e := range entries {
		want := fmt.Sprintf("n%d", total-1-i)
		if e.Target != want {
			t.Fatalf("entries[%d].Target = %q, want %q (newest first)", i, e.Target, want)
		}
	}
	// A bounded backward read of 50 lines out of 50000 completes in
	// milliseconds; a full forward scan of the file would too on most
	// hardware, but comfortably inside this generous ceiling either way --
	// this is a coarse regression guard, not a tight benchmark.
	if elapsed > 2*time.Second {
		t.Fatalf("Recent(%d) on a %d-line file took %s, want well under 2s", limit, total, elapsed)
	}
}

// TestAuditLogRecentEdgeCases is a table test of the backward-chunked Recent():
// each case writes a raw fixture directly (not through Append) so the byte
// layout (trailing newline or not, a line straddling a chunk boundary) is
// fully controlled, and asserts the EXACT newest-first Action sequence.
func TestAuditLogRecentEdgeCases(t *testing.T) {
	// line builds one raw JSONL line (no trailing newline of its own --
	// callers join with "\n" or append one explicitly) for action act.
	line := func(ts int64, act string) string {
		return fmt.Sprintf(`{"ts":%d,"actor":"cli","action":%q}`, ts, act)
	}

	tests := []struct {
		name        string
		content     string
		chunkSize   int64 // 0 = leave the package default in place
		limit       int
		wantActions []string // newest first
	}{
		{
			// Forces several backward doublings (8 -> 32 -> 128 -> ...)
			// before a single read window covers enough complete lines to
			// satisfy limit=10 against a file no single 8-byte read could
			// ever hold even one whole line of.
			name:        "multi-iteration chunk growth",
			content:     joinLines(line, 10),
			chunkSize:   8,
			limit:       10,
			wantActions: reversedActions(10),
		},
		{
			name:        "no trailing newline",
			content:     line(1000, "a0") + "\n" + line(1001, "a1") + "\n" + line(1002, "a2"), // last line has NO trailing \n
			limit:       10,
			wantActions: []string{"a2", "a1", "a0"},
		},
		{
			name:        "empty zero-byte file",
			content:     "",
			limit:       10,
			wantActions: nil,
		},
		{
			name:        "limit larger than the file",
			content:     joinLines(line, 5),
			limit:       1000,
			wantActions: reversedActions(5),
		},
		{
			// Each line here is a fixed 40 bytes on the wire (39 bytes of
			// JSON + "\n"); chunkSize=25 is smaller than that, so every
			// backward read's start offset falls strictly INSIDE a line
			// rather than landing on a line boundary -- exercising the
			// "drop the partial leading line, re-read a larger window on
			// the next step" path multiple times (verified by hand: the
			// 25-byte and 100-byte reads each land mid-line and are
			// discarded before the final whole-file read succeeds) before
			// the line that spans each of those boundaries is ever
			// returned whole.
			name:        "line spanning a chunk boundary",
			content:     joinLines(line, 5),
			chunkSize:   25,
			limit:       5,
			wantActions: reversedActions(5),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.chunkSize != 0 {
				orig := auditRecentChunkSize
				auditRecentChunkSize = tc.chunkSize
				defer func() { auditRecentChunkSize = orig }()
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "audit.jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			a := newAuditLog(path)
			entries, err := a.Recent(tc.limit)
			if err != nil {
				t.Fatalf("Recent(%d): %v", tc.limit, err)
			}
			gotActions := make([]string, 0, len(entries))
			for _, e := range entries {
				gotActions = append(gotActions, e.Action)
			}
			if !equalStrSlices(gotActions, tc.wantActions) {
				t.Fatalf("Recent(%d) actions = %v, want %v", tc.limit, gotActions, tc.wantActions)
			}
		})
	}
}

// joinLines builds n lines (ts=1000+i, action="aI") via mk, joined with "\n"
// plus a final trailing "\n" -- TestAuditLogRecentEdgeCases' well-formed
// fixture shape.
func joinLines(mk func(ts int64, act string) string, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(mk(int64(1000+i), fmt.Sprintf("a%d", i)))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// reversedActions returns ["a<n-1>", ..., "a1", "a0"] -- joinLines' actions,
// newest (highest ts / last-written) first, matching Recent's own contract.
func reversedActions(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("a%d", n-1-i)
	}
	return out
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
