// Package trinetra: fleet_silences.go is the master's silence and
// maintenance-window store: explicit silences and recurring
// maintenance windows, both persisted to silences.json, both suppressing a
// matching alert exactly the same way (a maintenance window's reason is
// simply "maintenance <name>"). It also builds the "silences" stream frame
// pushed to children (fleet_engine.go's TickSilences/PushSilencesNow) and,
// at the bottom of the file, the child-side counterpart that stores and
// applies whatever the master last pushed.
package trinetra

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// silenceExpiredPruneAfter is how long past its End a silence is kept before
// TickSilences prunes it (global-constraints: 7 days).
const silenceExpiredPruneAfter = 7 * 24 * time.Hour

// silencePushInterval is how often TickSilences refreshes every connected
// node's pushed silence set unconditionally, so a maintenance window's
// next-24h expansion stays current even when no silence ever changes.
const silencePushInterval = 10 * time.Minute

// randomSilenceID returns 48 random bits as 12 hex chars, matching
// randomIncidentID's shape.
func randomSilenceID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// validateMatchers rejects an empty matcher list, one containing a completely
// empty Matcher (either would match every alert on every node, which is never
// intentional), or one whose Node/Rule glob doesn't parse (the same check as
// validGlob for a Route's matchers).
func validateMatchers(ms []core.Matcher) error {
	if len(ms) == 0 {
		return errors.New("a silence must match something")
	}
	for _, m := range ms {
		if m.Empty() {
			return errors.New("a silence must match something")
		}
		if m.Node != "" && !validGlob(m.Node) {
			return fmt.Errorf("invalid node glob %q", m.Node)
		}
		if m.Rule != "" && !validGlob(m.Rule) {
			return fmt.Errorf("invalid rule glob %q", m.Rule)
		}
	}
	return nil
}

func matchersApply(ms []core.Matcher, nodeID, nodeName string, tags []string, rule, severity string) bool {
	for _, m := range ms {
		if m.Matches(nodeID, nodeName, tags, rule, severity) {
			return true
		}
	}
	return false
}

// applicableMatchers returns the SUBSET of ms whose Node/Tag constraints could
// apply to this node. A Silence/Maintenance's Matchers are OR'd, but only some
// may be "meant for" this node: pushing the whole list to every node any single
// matcher applies to would leak an unrelated matcher (e.g. one for a different
// node, with no Rule/Severity of its own) to a node it was never meant to
// cover. Only the MASTER filters, resolving nodeID/nodeName from its registry
// (names are unique), so this is a precise one-node decision; the child
// (pushedSilences.Suppressed) trusts the result verbatim.
func applicableMatchers(ms []core.Matcher, nodeID, nodeName string, tags []string) []core.Matcher {
	var out []core.Matcher
	for _, m := range ms {
		if m.CouldApplyToNode(nodeID, nodeName, tags) {
			out = append(out, m)
		}
	}
	return out
}

// validHHMM wraps the shared parseHHMM (digest.go) with the range check it
// doesn't itself do (its Sscanf("%d:%d") happily accepts "99:99").
func validHHMM(s string) bool {
	h, m, ok := parseHHMM(s)
	return ok && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

func validateMaintenance(m core.Maintenance) error {
	if err := validateMatchers(m.Matchers); err != nil {
		return err
	}
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("a maintenance window needs a name")
	}
	if len(m.Weekdays) == 0 {
		return errors.New("a maintenance window needs at least one weekday")
	}
	for _, wd := range m.Weekdays {
		if wd < 0 || wd > 6 {
			return fmt.Errorf("invalid weekday %d (want 0=Sunday..6=Saturday)", wd)
		}
	}
	if !validHHMM(m.From) {
		return fmt.Errorf("invalid --from %q (want HH:MM)", m.From)
	}
	if !validHHMM(m.To) {
		return fmt.Errorf("invalid --to %q (want HH:MM)", m.To)
	}
	if _, err := time.LoadLocation(m.TZ); err != nil {
		return fmt.Errorf("invalid --tz %q: %w", m.TZ, err)
	}
	return nil
}

func validateSilence(s core.Silence) error {
	if err := validateMatchers(s.Matchers); err != nil {
		return err
	}
	if s.End <= s.Start {
		return errors.New("a silence's end must be after its start")
	}
	return nil
}

// occurrence is one concrete [Start,End) instant a maintenance window's
// recurring definition expands to; an alias for core.Occurrence.
type occurrence = core.Occurrence

// maintenanceOccurrencesInRange returns every occurrence of m that overlaps
// [from, until), expanded in m's own TZ: a thin wrapper over
// core.MaintenanceOccurrences, which holds the one implementation so
// internal/web's silences page can compute the SAME "next occurrence" without
// importing internal/trinetra (the module graph is deliberately one-way). See
// core.MaintenanceOccurrences for the DST-safety rationale and the
// crossing-midnight ownership rule.
func maintenanceOccurrencesInRange(m core.Maintenance, from, until time.Time) []occurrence {
	return core.MaintenanceOccurrences(m, from, until)
}

// maintenanceActiveAt reports whether m is active at t (in m's own TZ).
func maintenanceActiveAt(m core.Maintenance, t time.Time) bool {
	for _, occ := range maintenanceOccurrencesInRange(m, t, t.Add(time.Second)) {
		if occ.Start <= t.Unix() && t.Unix() < occ.End {
			return true
		}
	}
	return false
}

// suppressionInfo is why fleetAlertEngine.Submit decided not to deliver an
// alert: an active silence or maintenance window, identified by a
// human-readable reason ("silence <id> by <author>" / "maintenance <name>").
type suppressionInfo struct {
	Reason string
}

// pushedSilence is one entry in the "silences" stream frame's Data
// (global-constraints: `{"silences":[{matchers,start,end,id,reason}]}`):
// either an explicit active silence verbatim, or one concrete occurrence of
// a maintenance window, already expanded to a real [Start,End) instant so
// the child never needs to know about weekdays/TZ/crossing-midnight at all.
type pushedSilence struct {
	Matchers []core.Matcher `json:"matchers"`
	Start    int64          `json:"start"`
	End      int64          `json:"end"`
	ID       string         `json:"id"`
	Reason   string         `json:"reason"`
}

type silencesFrameData struct {
	Silences []pushedSilence `json:"silences"`
}

// --- master-side store -------------------------------------------------

// silencesFileV1 is silences.json's on-disk shape.
type silencesFileV1 struct {
	Silences     []core.Silence     `json:"silences"`
	Maintenances []core.Maintenance `json:"maintenances"`
}

// silenceStore is the master's silence/maintenance-window store, persisted
// atomically (temp + rename + fsync, via writeFileAtomicSynced) to one JSON file,
// 0600.
type silenceStore struct {
	path string

	mu           sync.Mutex
	silences     []core.Silence
	maintenances []core.Maintenance
}

// loadSilenceStore opens (or creates) the store at path. A missing file is
// not an error: a fresh master has no silences yet.
func loadSilenceStore(path string) (*silenceStore, error) {
	s := &silenceStore{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var data silencesFileV1
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("fleet: parse %s: %w", path, err)
	}
	s.silences, s.maintenances = data.Silences, data.Maintenances
	return s, nil
}

func (s *silenceStore) saveLocked() error {
	b, err := json.MarshalIndent(silencesFileV1{Silences: s.silences, Maintenances: s.maintenances}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(s.path, b, 0o600)
}

// List returns every silence, in creation order.
func (s *silenceStore) List() []core.Silence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Silence(nil), s.silences...)
}

// Create validates and stores sil, assigning it an ID.
func (s *silenceStore) Create(sil core.Silence) (core.Silence, error) {
	if err := validateSilence(sil); err != nil {
		return core.Silence{}, err
	}
	id, err := randomSilenceID()
	if err != nil {
		return core.Silence{}, err
	}
	sil.ID = id
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silences = append(s.silences, sil)
	if err := s.saveLocked(); err != nil {
		s.silences = s.silences[:len(s.silences)-1]
		return core.Silence{}, err
	}
	return sil, nil
}

// Expire pulls id's End back to now, if it isn't already <= now.
func (s *silenceStore) Expire(id string, now int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.silences {
		if s.silences[i].ID != id {
			continue
		}
		if s.silences[i].End <= now {
			return nil // already over; nothing to do
		}
		prev := s.silences[i].End
		s.silences[i].End = now
		if err := s.saveLocked(); err != nil {
			s.silences[i].End = prev
			return err
		}
		return nil
	}
	return fmt.Errorf("no such silence %q: %w", id, core.ErrNotFound)
}

// Maintenances returns every maintenance window, in creation order.
func (s *silenceStore) Maintenances() []core.Maintenance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Maintenance(nil), s.maintenances...)
}

// SaveMaintenance validates and stores m: a new window (fresh ID) if m.ID is
// "", otherwise an in-place update of the existing window with that ID.
func (s *silenceStore) SaveMaintenance(m core.Maintenance) (core.Maintenance, error) {
	if err := validateMaintenance(m); err != nil {
		return core.Maintenance{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		id, err := randomSilenceID()
		if err != nil {
			return core.Maintenance{}, err
		}
		m.ID = id
		s.maintenances = append(s.maintenances, m)
		if err := s.saveLocked(); err != nil {
			s.maintenances = s.maintenances[:len(s.maintenances)-1]
			return core.Maintenance{}, err
		}
		return m, nil
	}
	for i, mm := range s.maintenances {
		if mm.ID != m.ID {
			continue
		}
		prev := s.maintenances[i]
		s.maintenances[i] = m
		if err := s.saveLocked(); err != nil {
			s.maintenances[i] = prev
			return core.Maintenance{}, err
		}
		return m, nil
	}
	return core.Maintenance{}, fmt.Errorf("no such maintenance %q: %w", m.ID, core.ErrNotFound)
}

// DeleteMaintenance removes maintenance window id.
func (s *silenceStore) DeleteMaintenance(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.maintenances {
		if m.ID != id {
			continue
		}
		prev := s.maintenances
		s.maintenances = slices.Delete(slices.Clone(s.maintenances), i, i+1)
		if err := s.saveLocked(); err != nil {
			s.maintenances = prev
			return err
		}
		return nil
	}
	return fmt.Errorf("no such maintenance %q: %w", id, core.ErrNotFound)
}

// Prune drops every silence expired for more than silenceExpiredPruneAfter,
// persisting the change only if something was actually removed. It never
// touches maintenance windows (recurring; there is nothing to expire).
func (s *silenceStore) Prune(now int64) {
	cutoff := now - int64(silenceExpiredPruneAfter/time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	var kept []core.Silence
	changed := false
	for _, sil := range s.silences {
		if sil.End < cutoff {
			changed = true
			continue
		}
		kept = append(kept, sil)
	}
	if !changed {
		return
	}
	s.silences = kept
	_ = s.saveLocked()
}

// Suppressed reports whether an alert with the given rule (its Key) and
// severity, on the node identified by its id/display name/tags ("" / nil
// for a master-own alert), is currently covered by an active silence or
// maintenance window at unix time now. Explicit silences are checked first.
func (s *silenceStore) Suppressed(now int64, nodeID, nodeName string, tags []string, rule, severity string) *suppressionInfo {
	s.mu.Lock()
	silences := append([]core.Silence(nil), s.silences...)
	maints := append([]core.Maintenance(nil), s.maintenances...)
	s.mu.Unlock()

	for _, sil := range silences {
		if sil.Start > now || sil.End <= now {
			continue
		}
		if matchersApply(sil.Matchers, nodeID, nodeName, tags, rule, severity) {
			return &suppressionInfo{Reason: fmt.Sprintf("silence %s by %s", sil.ID, sil.Author)}
		}
	}
	tNow := time.Unix(now, 0)
	for _, m := range maints {
		if !matchersApply(m.Matchers, nodeID, nodeName, tags, rule, severity) {
			continue
		}
		if maintenanceActiveAt(m, tNow) {
			return &suppressionInfo{Reason: "maintenance " + m.Name}
		}
	}
	return nil
}

// silencesForNode builds the filtered "silences" frame payload for a node with
// the given id/display name/tags: every currently active explicit silence with
// at least one matcher that could apply to it (only THOSE matchers are sent),
// plus every occurrence of a maintenance window (likewise filtered) in the next
// 24h, each expanded to a concrete [Start,End) instant. The master is the only
// place Node is resolved/matched; the child applies the result verbatim.
func (s *silenceStore) silencesForNode(now int64, nodeID, nodeName string, tags []string) []pushedSilence {
	s.mu.Lock()
	silences := append([]core.Silence(nil), s.silences...)
	maints := append([]core.Maintenance(nil), s.maintenances...)
	s.mu.Unlock()

	var out []pushedSilence
	for _, sil := range silences {
		if sil.Start > now || sil.End <= now {
			continue
		}
		applicable := applicableMatchers(sil.Matchers, nodeID, nodeName, tags)
		if len(applicable) == 0 {
			continue
		}
		out = append(out, pushedSilence{
			Matchers: applicable, Start: sil.Start, End: sil.End, ID: sil.ID,
			Reason: fmt.Sprintf("silence %s by %s", sil.ID, sil.Author),
		})
	}
	from := time.Unix(now, 0)
	until := from.Add(24 * time.Hour)
	for _, m := range maints {
		applicable := applicableMatchers(m.Matchers, nodeID, nodeName, tags)
		if len(applicable) == 0 {
			continue
		}
		for _, occ := range maintenanceOccurrencesInRange(m, from, until) {
			out = append(out, pushedSilence{
				Matchers: applicable, Start: occ.Start, End: occ.End, ID: m.ID,
				Reason: "maintenance " + m.Name,
			})
		}
	}
	return out
}

// --- child-side pushed-silence store -------------------------------------

// childSilencesPath is the sidecar's path under the daemon's state
// directory: <stateDir>/fleet-child/silences.json.
func childSilencesPath(stateDir string) string {
	return filepath.Join(fleetChildDir(stateDir), "silences.json")
}

// pushedSilences is the child's in-memory copy of the master's last pushed
// "silences" frame, also persisted to childSilencesPath (0600) so a restart
// while the master stays unreachable keeps honouring it. It is applied ONLY
// to a fallback delivery (handoff.Tick's overdue alerts, via
// deliverFallback): an ordinary local alert on a child that has never
// routed anything to the master is unaffected by anything the master ever
// pushed.
//
// The child never re-derives Node/Tag applicability: an
// earlier round added a Node re-check keyed off this child's own
// config.ServerName, but that name is not reliably the same string the
// master's registry has for this node (a fresh join's `--name` is never
// written back into server.name, and node names were not even unique on
// the master until this round) -- so the re-check could reject an entry the
// master correctly meant for this exact node, silently letting a SILENCED
// alert fall back and deliver. The master is the only place with the
// authoritative registry to resolve Node
// against, and it already filters `silencesForNode` down to exactly the
// matchers that apply to THIS node before ever sending them -- the child
// simply applies whatever it was pushed.
type pushedSilences struct {
	path string

	mu       sync.Mutex
	silences []pushedSilence
}

// newPushedSilences builds an empty pushedSilences (no push received yet).
func newPushedSilences(path string) *pushedSilences {
	return &pushedSilences{path: path}
}

// loadPushedSilences restores the sidecar written by a previous process. A
// missing or corrupt file just starts empty -- exactly like never having
// received a push.
func loadPushedSilences(path string) *pushedSilences {
	p := &pushedSilences{path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	var data silencesFrameData
	if json.Unmarshal(b, &data) == nil {
		p.silences = data.Silences
	}
	return p
}

// pushedSilencesWriteHook, when set by a test, runs synchronously right before
// Set's disk write, to prove the write happens while p.mu is held by blocking
// one caller mid-write and observing whether a second Set can race ahead.
var pushedSilencesWriteHook func()

// Set replaces the pushed silence set and persists it (0600, fsync'd). A nil
// receiver is a no-op (mirrors AlertLog/auditLog's nil-degrades-gracefully
// convention). p.mu is held for the entire call, including the disk write:
// otherwise two concurrent Set calls can complete out of order (the write
// for an earlier Set landing on disk after a later Set's write), leaving
// the file stale relative to p.silences.
func (p *pushedSilences) Set(silences []pushedSilence) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.silences = silences
	b, err := json.Marshal(silencesFrameData{Silences: silences})
	if err != nil {
		return err
	}
	if pushedSilencesWriteHook != nil {
		pushedSilencesWriteHook()
	}
	return writeFileAtomicSynced(p.path, b, 0o600)
}

// Suppressed reports whether a fallback delivery for an alert with the given
// rule (its Key) and severity is currently covered by a pushed silence or
// maintenance occurrence, and if so, its reason.
//
// Only Rule/Severity are checked: Node/Tag applicability was decided at the
// master, over its own registry, before the entry was pushed, and the child has
// no more authoritative source to re-derive it from (see pushedSilences).
//
// A nil receiver reports no suppression (no push ever received).
func (p *pushedSilences) Suppressed(now int64, rule, severity string) (string, bool) {
	if p == nil {
		return "", false
	}
	p.mu.Lock()
	silences := p.silences
	p.mu.Unlock()
	for _, s := range silences {
		if s.Start > now || s.End <= now {
			continue
		}
		for _, m := range s.Matchers {
			if m.Rule != "" {
				if ok, _ := path.Match(m.Rule, rule); !ok {
					continue
				}
			}
			if m.Severity != "" && !strings.EqualFold(m.Severity, severity) {
				continue
			}
			return s.Reason, true
		}
	}
	return "", false
}
