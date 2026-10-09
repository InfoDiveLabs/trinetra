package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// AuditRecord is one line of <StateDir>/audit.jsonl: who changed what, and
// what it was before/after. Written for every config write, channel CRUD
// mutation, alert ack and user-management mutation -- see appendAudit's
// callers (handlers_config.go, handlers_channels.go, handlers_alerts.go,
// handlers_users.go).
type AuditRecord struct {
	// Time is a Unix timestamp (seconds), defaulted to time.Now() by
	// appendAudit when left zero.
	Time int64 `json:"time"`
	// User is the acting session's account name (auditUser), or "" if no
	// session was resolved (shouldn't happen for a mutation already behind
	// requireRole, but this is display-only, not an access-control decision,
	// so it degrades gracefully rather than failing the write).
	User string `json:"user"`
	// Action names the mutation kind, e.g. "config.set", "channel.add",
	// "channel.remove", "channel.update", "alert.ack", "user.role",
	// "user.remove", "user.invite", "user.credential.revoke".
	Action string `json:"action"`
	// Key identifies what was changed within Action (a config dotted key, a
	// channel name, an alert key, a user id) -- omitted when Action itself is
	// the whole story (e.g. "user.invite" has no single prior "key").
	Key string `json:"key,omitempty"`
	// Old/New are the human-readable before/after values, omitted when not
	// applicable (e.g. a create/remove only needs one side).
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// auditLogPath is <StateDir>/audit.jsonl.
func auditLogPath(stateDir string) string {
	return filepath.Join(stateDir, "audit.jsonl")
}

// appendAudit appends rec as one JSONL line to <StateDir>/audit.jsonl,
// creating the state directory and file as needed. Mirrors
// internal/trinetra's own appendJSONL (alertlog.go/store.go): a single
// os.OpenFile(O_APPEND)+Write of one short line is atomic against
// interleaving from concurrent appenders on the same filesystem (POSIX
// guarantees a single write() to an O_APPEND-opened fd is atomic for writes
// under PIPE_BUF), so no additional locking is needed here.
//
// stateDir == "" (no state directory configured, e.g. some minimal test
// Deps) is treated as "nowhere to write" and silently succeeds -- the audit
// trail is a best-effort record of what happened, not the source of truth
// for any access-control decision, so its absence must never block or fail
// the mutation it's describing.
func appendAudit(stateDir string, rec AuditRecord) error {
	if stateDir == "" {
		return nil
	}
	if rec.Time == 0 {
		rec.Time = time.Now().Unix()
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(auditLogPath(stateDir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// auditUser resolves the acting request's session into the display name an
// AuditRecord.User should carry (userFromContext, middleware.go), or "" for
// a request with no resolved user.
func auditUser(r *http.Request) string {
	if u, ok := userFromContext(r); ok {
		return u.Name
	}
	return ""
}

// logAudit is the shared call-site helper every mutation handler uses: it
// fills Time/User from r and best-effort appends under d.StateDir, never
// surfacing a write failure to the caller (see appendAudit's doc -- the audit
// trail must never block the mutation it's describing).
func logAudit(d Deps, r *http.Request, action, key, old, new string) {
	_ = appendAudit(d.StateDir, AuditRecord{
		User:   auditUser(r),
		Action: action,
		Key:    key,
		Old:    old,
		New:    new,
	})
}
