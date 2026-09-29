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

// FloorVersion returns the parsed Floor, or fallback if Floor is unset or
// unparsable, or lower than fallback.
func (s State) FloorVersion(fallback Version) Version {
	if v, err := ParseVersion(s.Floor); err == nil {
		if CompareVersions(v, fallback) > 0 {
			return v
		}
	}
	return fallback
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
