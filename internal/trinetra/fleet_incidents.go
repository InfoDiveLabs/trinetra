// Package trinetra: fleet_incidents.go is the master's incident store: one
// incident per (node, alert key) fire->recover episode (grouping several
// alerts into one incident arrives in a later task). incidents.jsonl is
// append-only: every state change appends a full snapshot line, and on load
// the last line per id wins. Rotated at 50 MB to incidents.jsonl.1.
package trinetra

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// incidentRotateBytes is the incidents.jsonl size at which a fresh append rotates the file
// to incidents.jsonl.1 first (global-constraints: rotate at 50 MB).
const incidentRotateBytes = 50 << 20

// randomIncidentID returns 48 random bits as 12 hex chars.
func randomIncidentID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// incidentGroupKey identifies one (node, alert key) episode. nodeID is ""
// for the master's own alerts (fleet:node:*:down and friends).
func incidentGroupKey(nodeID, key string) string {
	if nodeID == "" {
		return "self:" + key
	}
	return nodeID + ":" + key
}

// incidentApply is what fleetAlertEngine.Submit hands to incidentStore.Apply for one
// dedup'd alert record.
type incidentApply struct {
	src              alertSource
	alert            Alert
	firedAt          int64
	deliveredLocally bool
	// suppressed is set when a silence or active maintenance window covers
	// this alert record: it is recorded as a "suppressed" timeline event
	// (leg-labelled "fire: "/"recover: " for a master-own incident, so
	// legDeliveredStatus's resurrection check treats it as handled) and the
	// incident's State becomes "suppressed" instead of "firing" (fire only;
	// a suppressed recover still resolves the incident normally).
	suppressed *suppressionInfo
	now        int64

	// groupKey, when non-empty, is the grouping bucket this alert joins/opens, computed by
	// fleetAlertEngine.groupKeyFor.
	groupKey string
	// dependencyFold, when non-empty, means this FIRE is a node-down alert folded into an
	// already-open incident because one of its node's dependencies is down.
	dependencyFold string
}

// suppressedDetail formats u.suppressed's reason for the incident timeline: leg-labelled
// ("fire: "/"recover: ") for a master-own alert.
func suppressedDetail(u incidentApply) string {
	if u.src.NodeID == "" {
		return legLabel(u.alert) + ": " + u.suppressed.Reason
	}
	return u.suppressed.Reason
}

// memberKey identifies one (node, alert key) member slot, independent of which
// incident/grouping-bucket it currently lives in -- see incidentStore.openMember.
type memberKey struct{ node, key string }

// incidentStore is the master's durable incident history: an in-memory
// index (byID, open) kept consistent with an append-only JSONL file on disk.
type incidentStore struct {
	path string

	mu   sync.Mutex
	byID map[string]core.Incident
	// open maps a still-firing-or-acked-or-suppressed incident's group key to its id, so a
	// later FIRE for the same bucket joins it instead of opening a duplicate.
	open map[string]string
	// openMember maps a (node, key) member slot to the id of the incident CURRENTLY holding
	// its unresolved instance.
	openMember map[memberKey]string
	// suppressed indexes every incident with AT LEAST ONE open (unresolved) member whose
	// SilencedBy is set (per-member, not keyed off the incident's State).
	suppressed map[string]struct{}
}

// loadIncidentStore opens (or creates) the incident store at path, replaying every line
// already on disk (a missing file is not an error: a fresh master has none yet).
func loadIncidentStore(path string) (*incidentStore, error) {
	s := &incidentStore{path: path, byID: map[string]core.Incident{}, open: map[string]string{}, openMember: map[memberKey]string{}, suppressed: map[string]struct{}{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadIncidentsRaw parses one incidents.jsonl-shaped file into a last-line-per-id map.
func loadIncidentsRaw(path string) (map[string]core.Incident, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]core.Incident{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var inc core.Incident
		if json.Unmarshal(line, &inc) != nil {
			continue // a corrupt line is skipped, not fatal
		}
		out[inc.ID] = inc // later lines for the same id overwrite earlier ones
	}
	return out, sc.Err()
}

// load reconstructs byID/open from disk: the previous rotation (path+".1") first, keeping
// only its STILL-OPEN incidents.
func (s *incidentStore) load() error {
	prev, err := loadIncidentsRaw(s.path + ".1")
	if err != nil {
		return err
	}
	for id, inc := range prev {
		if isOpenState(inc.State) {
			s.byID[id] = inc
		}
	}
	cur, err := loadIncidentsRaw(s.path)
	if err != nil {
		return err
	}
	for id, inc := range cur {
		s.byID[id] = inc
	}
	for id, inc := range s.byID {
		if isOpenState(inc.State) {
			s.open[inc.GroupKey] = id
		}
		if hasSilencedOpenMember(inc) {
			s.suppressed[id] = struct{}{}
		}
		for _, al := range inc.Alerts {
			if al.ResolvedAt == 0 {
				s.openMember[memberKey{al.Node, al.Key}] = id
			}
		}
	}
	return nil
}

// SuppressedOpen returns every incident with at least one open (unresolved) silenced
// member, from the maintained index rather than a full scan of byID.
func (s *incidentStore) SuppressedOpen() []core.Incident {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.Incident, 0, len(s.suppressed))
	for id := range s.suppressed {
		if inc, ok := s.byID[id]; ok {
			out = append(out, cloneIncident(inc))
		}
	}
	return out
}

// isOpenState reports whether an incident in this state is still "open" --
// tracked in s.open so a later fire/recover for the same (node, key) updates
// it rather than opening a duplicate. A suppressed incident is open exactly
// like a firing one: it is still an active episode, just not delivered.
func isOpenState(state string) bool {
	return state == "firing" || state == "acked" || state == "suppressed"
}

// seenKeys returns the (node, key, firedAt) dedup key for every alert ever recorded, read
// directly from BOTH the current file and its previous rotation (path+".1").
func (s *incidentStore) seenKeys() map[alertDedupKey]struct{} {
	out := map[alertDedupKey]struct{}{}
	for _, path := range [2]string{s.path + ".1", s.path} {
		incs, err := loadIncidentsRaw(path)
		if err != nil {
			continue
		}
		for _, inc := range incs {
			for _, a := range inc.Alerts {
				out[alertDedupKey{node: a.Node, key: a.Key, firedAt: a.FiredAt}] = struct{}{}
				// A resolved alert's ResolvedAt is a SECOND, separately
				// dedup'd alert record (the recover, whose own fired_at is
				// ResolvedAt -- see Apply's recover branch, which updates
				// the fire's own IncidentAlert in place rather than
				// appending a new one). Both must be seeded, or a restarted
				// engine would treat a replayed recover record as
				// never-before-seen and redeliver it.
				if a.ResolvedAt != 0 && a.ResolvedAt != a.FiredAt {
					out[alertDedupKey{node: a.Node, key: a.Key, firedAt: a.ResolvedAt}] = struct{}{}
				}
			}
		}
	}
	return out
}

// appendLine rotates path to path+".1" first if it has grown past
// incidentRotateBytes, then appends inc as one JSON line (fsync'd, like the
// rest of the fleet master's durable state).
func (s *incidentStore) appendLine(inc core.Incident) error {
	if fi, err := os.Stat(s.path); err == nil && fi.Size() >= incidentRotateBytes {
		_ = os.Rename(s.path, s.path+".1")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(inc)
	if err != nil {
		return err
	}
	return appendSynced(s.path, append(b, '\n'))
}

func nodesFor(src alertSource) []string {
	if src.NodeID == "" {
		return nil
	}
	return []string{src.NodeID}
}

// Apply folds one dedup'd alert record into the incident it belongs to, opening a new one
// on a "fire" with no open incident for the same (node, key).
func (s *incidentStore) Apply(u incidentApply) (core.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	leg := legLabel(u.alert)
	mk := memberKey{u.src.NodeID, u.alert.Key}

	if u.alert.Kind == "recover" {
		// A recover finds its OWN member by (node, key), via openMember -- NEVER by recomputing a
		// group key for the recover itself.
		id, hadOpenMember := s.openMember[mk]
		inc, ok := core.Incident{}, false
		if hadOpenMember {
			inc, ok = s.byID[id]
		}
		if !ok {
			gk := u.groupKey
			if gk == "" {
				gk = incidentGroupKey(u.src.NodeID, u.alert.Key)
			}
			inc, ok = s.lookupOpenLocked(gk)
			if !ok {
				// A recover with nothing open at all (restart lost it, or it raced a fire this process
				// never saw): record it anyway, already resolved, rather than dropping it silently.
				newID, err := randomIncidentID()
				if err != nil {
					return core.Incident{}, err
				}
				inc = core.Incident{ID: newID, GroupKey: gk, Title: u.alert.Title, Severity: u.alert.Severity.String(), Nodes: nodesFor(u.src), Opened: u.now}
			}
		}
		inc.Updated = u.now
		closed := false
		for i := range inc.Alerts {
			if inc.Alerts[i].Node == u.src.NodeID && inc.Alerts[i].Key == u.alert.Key && inc.Alerts[i].ResolvedAt == 0 {
				inc.Alerts[i].ResolvedAt = u.firedAt
				closed = true
				break
			}
		}
		if !closed {
			inc.Alerts = append(inc.Alerts, core.IncidentAlert{
				Node: u.src.NodeID, NodeName: u.src.NodeName, Key: u.alert.Key, Title: u.alert.Title, Severity: u.alert.Severity.String(),
				FiredAt: u.firedAt, ResolvedAt: u.firedAt, DeliveredLocally: u.deliveredLocally,
			})
		}
		inc.Timeline = append(inc.Timeline, core.IncidentEvent{
			TS: u.now, Kind: "resolved", Detail: recoverDetail(u), Actor: "system",
			Leg: leg, AlertKey: u.alert.Key, Node: u.src.NodeID, FiredAt: u.firedAt,
		})
		if u.deliveredLocally {
			inc.Timeline = append(inc.Timeline, core.IncidentEvent{
				TS: u.now, Kind: "delivered", Detail: "delivered locally by the node", Actor: "child",
				Leg: leg, AlertKey: u.alert.Key, Node: u.src.NodeID, FiredAt: u.firedAt,
			})
		}
		if u.suppressed != nil {
			inc.Timeline = append(inc.Timeline, core.IncidentEvent{
				TS: u.now, Kind: "suppressed", Detail: suppressedDetail(u), Actor: "system",
				Leg: leg, AlertKey: u.alert.Key, Node: u.src.NodeID, FiredAt: u.firedAt,
			})
		}
		// A "delivered" event for the master's OWN successful dispatch is appended later, by
		// AppendEvent, once delivery has actually completed.
		recomputeState(&inc, u.now)
		delete(s.openMember, mk) // this member is resolved: no longer "open" anywhere.
		s.syncIndexesLocked(inc)
		s.byID[inc.ID] = inc
		return inc, s.appendLine(inc)
	}

	// fire (or any other non-recover kind, treated like a fire)
	gk := u.groupKey
	if gk == "" {
		gk = incidentGroupKey(u.src.NodeID, u.alert.Key)
	}
	inc, ok := s.lookupOpenLocked(gk)
	if !ok {
		id, err := randomIncidentID()
		if err != nil {
			return core.Incident{}, err
		}
		inc = core.Incident{ID: id, GroupKey: gk, Title: u.alert.Title, Severity: u.alert.Severity.String(), Opened: u.now}
	}
	inc.Updated = u.now
	if u.src.NodeID != "" && !slices.Contains(inc.Nodes, u.src.NodeID) {
		inc.Nodes = append(inc.Nodes, u.src.NodeID)
	}
	silencedBy := ""
	if u.suppressed != nil {
		silencedBy = u.suppressed.Reason
	}
	inc.Alerts = append(inc.Alerts, core.IncidentAlert{
		Node: u.src.NodeID, NodeName: u.src.NodeName, Key: u.alert.Key, Title: u.alert.Title, Severity: u.alert.Severity.String(),
		FiredAt: u.firedAt, DeliveredLocally: u.deliveredLocally, Suppressed: u.dependencyFold, SilencedBy: silencedBy,
	})
	inc.Timeline = append(inc.Timeline, core.IncidentEvent{
		TS: u.now, Kind: "fired", Detail: fireDetail(u), Actor: "system",
		Leg: leg, AlertKey: u.alert.Key, Node: u.src.NodeID, FiredAt: u.firedAt,
	})
	if u.deliveredLocally {
		inc.Timeline = append(inc.Timeline, core.IncidentEvent{
			TS: u.now, Kind: "delivered", Detail: "delivered locally by the node", Actor: "child",
			Leg: leg, AlertKey: u.alert.Key, Node: u.src.NodeID, FiredAt: u.firedAt,
		})
	}
	if u.suppressed != nil {
		inc.Timeline = append(inc.Timeline, core.IncidentEvent{
			TS: u.now, Kind: "suppressed", Detail: suppressedDetail(u), Actor: "system",
			Leg: leg, AlertKey: u.alert.Key, Node: u.src.NodeID, FiredAt: u.firedAt,
		})
	}
	if u.dependencyFold != "" {
		inc.Timeline = append(inc.Timeline, core.IncidentEvent{
			TS: u.now, Kind: "suppressed", Detail: u.dependencyFold, Actor: "system",
			Leg: leg, AlertKey: u.alert.Key, Node: u.src.NodeID, FiredAt: u.firedAt,
		})
	}
	// A "delivered" event for the master's OWN successful dispatch is appended later, by
	// AppendEvent, once delivery has actually completed.
	recomputeState(&inc, u.now)
	s.openMember[mk] = inc.ID
	s.syncIndexesLocked(inc)
	s.byID[inc.ID] = inc
	return inc, s.appendLine(inc)
}

// recomputeState derives inc.State from its members and ack status, rather than from only
// the record just applied.
func recomputeState(inc *core.Incident, now int64) {
	if allAlertsResolved(inc.Alerts) {
		if inc.State != "resolved" {
			inc.Resolved = now
		}
		inc.State = "resolved"
		return
	}
	if inc.AckedBy != "" {
		inc.State = "acked"
		return
	}
	anyOpen, allSuppressed := false, true
	for _, al := range inc.Alerts {
		if al.ResolvedAt != 0 {
			continue
		}
		anyOpen = true
		if al.SilencedBy == "" && al.Suppressed == "" {
			allSuppressed = false
			break
		}
	}
	if anyOpen && allSuppressed {
		inc.State = "suppressed"
		return
	}
	inc.State = "firing"
}

// hasSilencedOpenMember reports whether inc has any unresolved member whose SilencedBy is
// set -- the predicate behind the incidentStore.suppressed index.
func hasSilencedOpenMember(inc core.Incident) bool {
	for _, al := range inc.Alerts {
		if al.ResolvedAt == 0 && al.SilencedBy != "" {
			return true
		}
	}
	return false
}

// syncIndexesLocked keeps s.open and s.suppressed consistent with inc's
// freshly recomputed State/members, after ANY mutation. Caller holds s.mu.
func (s *incidentStore) syncIndexesLocked(inc core.Incident) {
	if inc.State == "resolved" {
		delete(s.open, inc.GroupKey)
	} else {
		s.open[inc.GroupKey] = inc.ID
	}
	if hasSilencedOpenMember(inc) {
		s.suppressed[inc.ID] = struct{}{}
	} else {
		delete(s.suppressed, inc.ID)
	}
}

// allAlertsResolved reports whether every member alert has recovered -- an incident with 1+
// members resolves only once ALL of them have.
func allAlertsResolved(alerts []core.IncidentAlert) bool {
	if len(alerts) == 0 {
		return false
	}
	for _, a := range alerts {
		if a.ResolvedAt == 0 {
			return false
		}
	}
	return true
}

func fireDetail(u incidentApply) string {
	if u.src.NodeID != "" {
		return fmt.Sprintf("fired on %s", u.src.NodeName)
	}
	return "fired"
}

func recoverDetail(u incidentApply) string {
	if u.src.NodeID != "" {
		return fmt.Sprintf("recovered on %s", u.src.NodeName)
	}
	return "recovered"
}

// lookupOpenLocked returns the currently open incident for gk, if any.
func (s *incidentStore) lookupOpenLocked(gk string) (core.Incident, bool) {
	id, ok := s.open[gk]
	if !ok {
		return core.Incident{}, false
	}
	inc, ok := s.byID[id]
	return inc, ok
}

// AppendEvent appends ev to id's timeline and persists the updated snapshot.
func (s *incidentStore) AppendEvent(id string, ev core.IncidentEvent) (core.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inc, ok := s.byID[id]
	if !ok {
		return core.Incident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	inc.Timeline = append(inc.Timeline, ev)
	inc.Updated = ev.TS
	// State is always DERIVED from members, never set ad hoc here.
	recomputeState(&inc, ev.TS)
	s.syncIndexesLocked(inc)
	s.byID[id] = inc
	return inc, s.appendLine(inc)
}

// MarkDeliveredLocally records, on the specific IncidentAlert (node, key, firedAt)
// identifies, that the child ended up delivering it locally after all.
func (s *incidentStore) MarkDeliveredLocally(node, key string, firedAt, now int64) (inc core.Incident, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cand := range s.byID {
		for i := range cand.Alerts {
			al := &cand.Alerts[i]
			if al.Node != node || al.Key != key || al.FiredAt != firedAt {
				continue
			}
			if al.DeliveredLocally {
				return cand, true, nil // already recorded
			}
			al.DeliveredLocally = true
			cand.Updated = now
			// Leg/AlertKey/Node/FiredAt MUST be set here, exactly like every other "delivered" event
			// Apply/deliverGroup/deliverUnsilencedMember append: without them.
			cand.Timeline = append(cand.Timeline, core.IncidentEvent{
				TS: now, Kind: "delivered", Detail: "delivered locally by the node", Actor: "child",
				Leg: "fire", AlertKey: key, Node: node, FiredAt: firedAt,
			})
			recomputeState(&cand, now)
			s.syncIndexesLocked(cand)
			s.byID[id] = cand
			return cand, true, s.appendLine(cand)
		}
	}
	return core.Incident{}, false, nil
}

// Ack marks id acknowledged by actor and appends the resulting snapshot.
func (s *incidentStore) Ack(id, actor string, now int64) (core.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inc, ok := s.byID[id]
	if !ok {
		return core.Incident{}, fmt.Errorf("no such incident %q: %w", id, core.ErrNotFound)
	}
	if inc.State == "resolved" {
		return core.Incident{}, fmt.Errorf("incident %q is already resolved", id)
	}
	inc.AckedBy = actor
	inc.Updated = now
	inc.Timeline = append(inc.Timeline, core.IncidentEvent{TS: now, Kind: "acked", Actor: actor})
	recomputeState(&inc, now) // AckedBy set: recomputeState resolves this to "acked" (see its doc comment).
	s.syncIndexesLocked(inc)
	s.byID[id] = inc
	return inc, s.appendLine(inc)
}

// Get returns one incident by id.
func (s *incidentStore) Get(id string) (core.Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inc, ok := s.byID[id]
	return cloneIncident(inc), ok
}

// cloneIncident returns a copy of inc whose slice fields (Nodes, Alerts, Timeline) do NOT
// share a backing array with whatever is stored in s.byID.
func cloneIncident(inc core.Incident) core.Incident {
	inc.Nodes = append([]string(nil), inc.Nodes...)
	inc.Alerts = append([]core.IncidentAlert(nil), inc.Alerts...)
	inc.Timeline = append([]core.IncidentEvent(nil), inc.Timeline...)
	for i := range inc.Timeline {
		inc.Timeline[i].Channels = append([]string(nil), inc.Timeline[i].Channels...)
	}
	return inc
}

// OpenForGroupKey returns the currently open (firing or acked) incident for gk,
// if any, without a full alert-key scan.
func (s *incidentStore) OpenForGroupKey(gk string) (core.Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inc, ok := s.lookupOpenLocked(gk)
	return cloneIncident(inc), ok
}

// OpenAlertIncident returns the currently open incident holding an
// UNRESOLVED (Node, Key) member, if any -- for per-alert operations (ack,
// resurrection, unsilence, dependency release) that must locate a specific
// member without knowing (or recomputing) which grouping bucket it joined.
// If several unresolved instances of the same (node, key) exist across
// different open incidents (a re-fire whose earlier incident already
// resolved is impossible; a re-fire joining a DIFFERENT bucket than its
// still-open predecessor, e.g. after a route's GroupBy or severity changed
// mid-incident, is the only way this can happen), the most recently
// updated incident wins.
func (s *incidentStore) OpenAlertIncident(node, key string) (core.Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best core.Incident
	found := false
	for _, inc := range s.byID {
		if !isOpenState(inc.State) {
			continue
		}
		for _, a := range inc.Alerts {
			if a.Node != node || a.Key != key || a.ResolvedAt != 0 {
				continue
			}
			if !found || inc.Updated > best.Updated {
				best, found = inc, true
			}
			break
		}
	}
	return cloneIncident(best), found
}

// HasUnresolvedAlert reports whether any CURRENTLY OPEN incident holds an unresolved (node,
// key) member -- used by fleetAlertEngine's dependency check.
func (s *incidentStore) HasUnresolvedAlert(node, key string) bool {
	_, ok := s.OpenAlertIncident(node, key)
	return ok
}

// DeliverUnsilencedMember atomically clears one member's SilencedBy (unsilence decides
// using SilencedBy, never the timeline) and appends events.
func (s *incidentStore) DeliverUnsilencedMember(id, node, key string, firedAt int64, events ...core.IncidentEvent) (inc core.Incident, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(events) == 0 {
		return core.Incident{}, false, nil
	}
	cand, exists := s.byID[id]
	if !exists {
		return core.Incident{}, false, nil
	}
	found := false
	for i := range cand.Alerts {
		al := &cand.Alerts[i]
		if al.Node != node || al.Key != key || al.FiredAt != firedAt {
			continue
		}
		if al.ResolvedAt != 0 || al.SilencedBy == "" {
			return core.Incident{}, false, nil
		}
		al.SilencedBy = ""
		found = true
		break
	}
	if !found {
		return core.Incident{}, false, nil
	}
	var now int64
	for _, ev := range events {
		cand.Timeline = append(cand.Timeline, ev)
		if ev.TS > now {
			now = ev.TS
		}
	}
	cand.Updated = now
	recomputeState(&cand, now)
	s.syncIndexesLocked(cand)
	s.byID[id] = cand
	return cand, true, s.appendLine(cand)
}

// ReleaseFoldedMember clears a dependency-suppressed member's Suppressed reason
// once none of its node's dependencies are down, recording reason (e.g.
// "released: parent web-1 recovered") on the member and as a structured
// "released" timeline event. A no-op (ok=false) if id has no such unresolved,
// currently-suppressed (node, key) member (recovered or released already).
func (s *incidentStore) ReleaseFoldedMember(id, node, key string, firedAt, now int64, reason string) (inc core.Incident, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cand, exists := s.byID[id]
	if !exists {
		return core.Incident{}, false, nil
	}
	for i := range cand.Alerts {
		al := &cand.Alerts[i]
		if al.Node != node || al.Key != key || al.FiredAt != firedAt {
			continue
		}
		if al.ResolvedAt != 0 || al.Suppressed == "" {
			return core.Incident{}, false, nil
		}
		al.Suppressed = reason
		// This member's involvement HERE is over: it moves to a brand new incident of its own
		// (deliverReleasedMember), so its old entry is closed out now.
		al.ResolvedAt = now
		cand.Updated = now
		cand.Timeline = append(cand.Timeline, core.IncidentEvent{
			TS: now, Kind: "released", Detail: reason, Actor: "system",
			Leg: "fire", AlertKey: key, Node: node, FiredAt: firedAt,
		})
		recomputeState(&cand, now)
		// This exact (node, key) member is no longer open HERE -- its openMember entry, if it
		// still pointed at this incident, is cleared.
		if s.openMember[memberKey{node, key}] == id {
			delete(s.openMember, memberKey{node, key})
		}
		s.syncIndexesLocked(cand)
		s.byID[id] = cand
		return cand, true, s.appendLine(cand)
	}
	return core.Incident{}, false, nil
}

// nodeTagsFunc looks up a node's tags for IncidentFilter.Tag matching; nil
// (self/unknown) is treated as no tags.
type nodeTagsFunc func(nodeID string) []string

func incidentMatchesTag(inc core.Incident, tag string, tagsOf nodeTagsFunc) bool {
	if tag == "" {
		return true
	}
	for _, n := range inc.Nodes {
		if slices.Contains(tagsOf(n), tag) {
			return true
		}
	}
	return false
}

// List returns incidents matching filter, newest-updated first.
func (s *incidentStore) List(filter core.IncidentFilter, tagsOf nodeTagsFunc) []core.Incident {
	s.mu.Lock()
	all := make([]core.Incident, 0, len(s.byID))
	for _, inc := range s.byID {
		all = append(all, cloneIncident(inc))
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Updated > all[j].Updated })
	var out []core.Incident
	for _, inc := range all {
		if filter.State != "" && inc.State != filter.State {
			continue
		}
		if filter.Node != "" && !slices.Contains(inc.Nodes, filter.Node) {
			continue
		}
		if !incidentMatchesTag(inc, filter.Tag, tagsOf) {
			continue
		}
		out = append(out, inc)
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out
}

// FindByAlertKey returns the most recently updated incident containing an
// IncidentAlert with the given alert key, for `fleet explain <key>`.
func (s *incidentStore) FindByAlertKey(key string) (core.Incident, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best core.Incident
	found := false
	for _, inc := range s.byID {
		for _, a := range inc.Alerts {
			if a.Key != key {
				continue
			}
			if !found || inc.Updated > best.Updated {
				best, found = inc, true
			}
			break
		}
	}
	return cloneIncident(best), found
}
