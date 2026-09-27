// Package trinetra: fleet_audit.go is the fleet master's audit log: an
// append-only, 0600 record of every fleet mutation (rename, tag, revoke,
// remove, token create/delete, incident ack) with who did it.
package trinetra

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// auditLog appends AuditEntry lines to a single JSONL file.
type auditLog struct {
	mu   sync.Mutex
	path string
}

func newAuditLog(path string) *auditLog { return &auditLog{path: path} }

// Append records one audit entry. A nil *auditLog is a no-op (mirrors
// AlertLog's nil-degrades-gracefully convention), so tests and callers that
// have no audit log wired need not nil-check before calling.
func (a *auditLog) Append(actor, action, target, detail string, now int64) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if actor == "" {
		actor = "unknown"
	}
	b, err := json.Marshal(core.AuditEntry{TS: now, Actor: actor, Action: action, Target: target, Detail: detail})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	return appendSynced(a.path, append(b, '\n'))
}

// Recent returns the most recent audit entries, newest first, up to limit
// (<= 0 means unlimited). A missing file is not an error (nothing audited
// yet).
func (a *auditLog) Recent(limit int) ([]core.AuditEntry, error) {
	if a == nil {
		return nil, nil
	}
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []core.AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e core.AuditEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
