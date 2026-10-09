package update

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Pending describes an update install that is in progress or awaiting
// confirmation. Rollback marks a `trinetra update rollback` in progress; it
// never raises the floor and never marks a version bad.
type Pending struct {
	Version  string   `json:"version"`
	From     string   `json:"from"`
	Deadline int64    `json:"deadline"`
	Files    []string `json:"files"`
	Rollback bool     `json:"rollback,omitempty"`
	// Phase is "swapping" from just before the first binary is replaced
	// until every binary and plugins.json are in place, then "swapped"
	// (empty in state written before phases existed: treat as swapped).
	Phase string `json:"phase,omitempty"`
	// RestoreFailed is set by the guard's rollbackPending when a Pending
	// has failed its health gate AND restoring the previous build itself
	// then fails: the failure detail (for display -- see RestoreFailedReason
	// for the original cause alone). Pending is kept (not cleared) while
	// this is set, so the next watchdog tick retries the restore
	// (retryFailedRestore) instead of re-running the health gate; a
	// successful retry clears Pending (and this field) entirely. Only ever
	// set for a forward update (Rollback == false) -- a failed rollback
	// confirmation has no older build to retry against and never sets it.
	RestoreFailed string `json:"restore_failed,omitempty"`
	// RestoreFailedReason is the health-gate failure that started this
	// RestoreFailed episode (e.g. "trinetra.service is not active"), kept
	// separately from RestoreFailed so a later successful retry's
	// Result.Detail explains the real reason the update rolled back rather
	// than an ever-growing chain of "restoring also failed" text
	// accumulated across retries. Set once, alongside RestoreFailed, and
	// never recomputed by a retry (there is no health gate to re-derive it
	// from -- see retryFailedRestore).
	RestoreFailedReason string `json:"restore_failed_reason,omitempty"`
	// RestoreFailedNotified dedups the critical alert for RestoreFailed:
	// the daemon's update loop (notifyRestoreFailed) sets it once the alert
	// has been delivered, so repeated failing watchdog retries (every
	// minute, each a fresh guard process) do not re-alert on every tick.
	RestoreFailedNotified bool `json:"restore_failed_notified,omitempty"`
}

// Result records the outcome of the most recent update attempt.
type Result struct {
	Version  string `json:"version"`
	From     string `json:"from"`
	Outcome  string `json:"outcome"` // committed | rolled_back
	Detail   string `json:"detail,omitempty"`
	At       int64  `json:"at"`
	Notified bool   `json:"notified"`
}

// State is Trinetra's persisted self-update bookkeeping: the version floor
// (which never lowers), any pending update, versions known bad, and
// notification state.
type State struct {
	Floor             string   `json:"floor,omitempty"`
	Pending           *Pending `json:"pending,omitempty"`
	Bad               []string `json:"bad_versions,omitempty"`
	LastCheck         int64    `json:"last_check,omitempty"`
	LastPointerIssued string   `json:"last_pointer_issued,omitempty"`
	Available         string   `json:"available,omitempty"`
	// StaleNotified dedups the freeze ("no fresh signed pointer") alert:
	// set when it fires, cleared once a fresh pointer verifies, so one
	// episode alerts once even across daemon restarts.
	StaleNotified bool `json:"stale_notified,omitempty"`
	// LastCheckError is the reason the most recent channel check failed to
	// verify a pointer (e.g. a 404 with no update.github_token configured),
	// cleared once a check verifies one. It is what `update status` shows
	// and what the daemon's periodic loop compares against to log a
	// changed cause only once (R25) rather than every tick, which matters
	// most before LastPointerIssued is ever set: see freezeVerdict.
	LastCheckError    string  `json:"last_check_error,omitempty"`
	AvailableNotified string  `json:"available_notified,omitempty"`
	Last              *Result `json:"last,omitempty"`
	// Unnotified holds outcomes not yet delivered, oldest first (#142).
	Unnotified []Result `json:"unnotified,omitempty"`
}

// maxUnnotified caps Unnotified; the oldest outcomes are dropped first.
const maxUnnotified = 8

// RecordResult sets Last and queues r for notification.
func (s *State) RecordResult(r Result) {
	last := r
	s.Last = &last
	s.Unnotified = append(s.Unnotified, r)
	if n := len(s.Unnotified); n > maxUnnotified {
		s.Unnotified = append([]Result(nil), s.Unnotified[n-maxUnnotified:]...)
	}
}

const stateFile = "state.json"

// LoadState reads dir/state.json. A missing file is an empty state; any
// other read or parse error is returned, never mapped to empty, because an
// unreadable floor must not look like "no floor" (that would re-open
// downgrades).
func LoadState(dir string) (State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("update: %s: %w", filepath.Join(dir, stateFile), err)
	}
	return s, nil
}

// SaveState writes dir/state.json atomically: unique temp file, fsync,
// rename. The directory is created (and forced to) 0700, the file 0600.
func SaveState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+stateFile+"."+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, stateFile)); err != nil {
		os.Remove(tmp)
		return err
	}
	// The directory fsync is what makes the rename durable; a failure here
	// is returned so a caller never believes Pending/floor were persisted
	// when they may not survive a power loss.
	return SyncDir(dir)
}

// FloorVersion returns the effective floor version -- the persisted Floor,
// or fallback (typically the running version) if that's higher, or fallback
// if Floor is unset -- and whether a floor actually applies.
//
// No floor applies (hasFloor false) only when both are true: nothing is
// persisted in Floor, and fallback is the zero Version (no known running
// version -- a fresh host, or a serverwatch migration/old binary that
// couldn't report one). A persisted Floor is always enforced once set, even
// if that happens to be "0.0.0" (a real committed release): only the
// "nothing recorded at all" case means no lower bound. See Policy.HasFloor.
//
// A persisted Floor that is not a valid version is an error, never "no
// floor": LoadState already fails closed on unreadable/corrupt state.json,
// and a floor that parses as JSON but not as a version is the same kind of
// corruption -- silently treating it as unset would re-open downgrades on a
// host with an unknown running version. Every caller must refuse its
// operation on this error rather than proceed as if nothing were persisted.
func (s State) FloorVersion(fallback Version) (v Version, hasFloor bool, err error) {
	v, hasFloor = fallback, fallback != (Version{})
	if s.Floor == "" {
		return v, hasFloor, nil
	}
	pv, perr := ParseVersion(s.Floor)
	if perr != nil {
		return Version{}, false, fmt.Errorf("update: state.json floor %q is not a valid version", s.Floor)
	}
	hasFloor = true
	if CompareVersions(pv, v) > 0 {
		v = pv
	}
	return v, hasFloor, nil
}

// RaiseFloor sets the floor to v, unless the current floor is already >= v.
func (s *State) RaiseFloor(v Version) {
	if cur, err := ParseVersion(s.Floor); err == nil && CompareVersions(cur, v) >= 0 {
		return
	}
	s.Floor = v.String()
}

// IsBad reports whether v is listed as a known-bad version ("v" prefix
// ignored on both sides).
func (s State) IsBad(v string) bool {
	v = strings.TrimPrefix(v, "v")
	for _, b := range s.Bad {
		if strings.TrimPrefix(b, "v") == v {
			return true
		}
	}
	return false
}

// MarkBad records v as known-bad once ("v" prefix stripped).
func (s *State) MarkBad(v string) {
	if !s.IsBad(v) {
		s.Bad = append(s.Bad, strings.TrimPrefix(v, "v"))
	}
}

// AvailableOver returns Available only while it is newer than running.
// Available is recorded at check time, so once that release is installed
// (by apply or a manual install) it would otherwise keep being offered
// until the next check.
func (s State) AvailableOver(running Version) string {
	v, err := ParseVersion(s.Available)
	if err != nil || CompareVersions(v, running) <= 0 {
		return ""
	}
	return s.Available
}
