package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// AuditRecord is one line of <StateDir>/audit.jsonl: who changed what, and what it was
// before/after.
type AuditRecord struct {
	// Time is a Unix timestamp (seconds), defaulted to time.Now() by
	// appendAudit when left zero.
	Time int64 `json:"time"`
	// User is the acting session's account name (auditUser), or "" if no
	// session was resolved (shouldn't happen for a mutation already behind
	// requireRole, but this is display-only, not an access-control decision,
	// so it degrades gracefully rather than failing the write).
	User string `json:"user"`
	// Action names the mutation kind, e.g. "config.set", "channel.add", "channel.remove",
	// "channel.update", "alert.ack", "user.role", "user.remove", "user.invite".
	Action string `json:"action"`
	// Key identifies what was changed within Action (a config dotted key, a channel name, an
	// alert key, a user id) -- omitted when Action itself is the whole story.
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

// appendAudit appends rec as one JSONL line to <StateDir>/audit.jsonl, creating the state
// directory and file as needed.
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

// auditUser resolves the acting request's session into the display name an AuditRecord.User
// should carry (userFromContext, middleware.go), or "" for a request with no resolved user.
func auditUser(r *http.Request) string {
	if u, ok := userFromContext(r); ok {
		return u.Name
	}
	return ""
}

// logAudit is the shared call-site helper every mutation handler uses: it fills Time/User
// from r and best-effort appends under d.StateDir.
func logAudit(d Deps, r *http.Request, action, key, old, new string) {
	_ = appendAudit(d.StateDir, AuditRecord{
		User:   auditUser(r),
		Action: action,
		Key:    key,
		Old:    old,
		New:    new,
	})
}
