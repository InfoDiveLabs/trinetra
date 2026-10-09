// Package trinetra: update_audit.go records self-update events in an audit
// log: apply and rollback starts (with the CLI user or
// "socket" as actor) and the guard's commit/rollback outcomes. On a fleet
// master the entries go to the fleet audit log; otherwise to
// <state>/update/audit.jsonl, using the same append-only JSONL writer.
package trinetra

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// auditPath is the fleet master's audit log when this host is a master,
// otherwise the local update audit log.
func (p updatePaths) auditPath() string {
	if fi, err := os.Stat(fleetMasterDir(p.StateDir)); err == nil && fi.IsDir() {
		return filepath.Join(fleetMasterDir(p.StateDir), "audit.jsonl")
	}
	return filepath.Join(p.dir(), "audit.jsonl")
}

// auditUpdate appends one entry, best-effort: an audit write failure is
// logged but never fails the update it describes.
func auditUpdate(p updatePaths, actor, action, target, detail string, now time.Time) {
	if err := newAuditLog(p.auditPath()).Append(actor, action, target, detail, now.Unix()); err != nil {
		log.Printf("update: audit %s: %v", action, err)
	}
}

// cliActor names the operator behind a CLI apply/rollback: the sudo user
// when there is one, else the login user, else the uid.
func cliActor() string {
	for _, k := range []string{"SUDO_USER", "USER"} {
		if v := os.Getenv(k); v != "" {
			return "cli:" + v
		}
	}
	return "cli:uid" + strconv.Itoa(os.Getuid())
}

// guardActor is the actor of the guard's own commit/rollback entries.
const guardActor = "guard"

// socketActor is the actor for apply/rollback requested over the control
// socket (the web UI and trinetra-ctl); the web UI's own audit log names the
// signed-in user for the same action.
const socketActor = "socket"
