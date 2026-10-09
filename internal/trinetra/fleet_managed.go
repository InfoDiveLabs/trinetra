// Package trinetra: fleet_managed.go is the master's managed-config
// fragment store and push plus the child's counterpart
// that applies whatever the master last pushed, live, with no restart, and
// makes the ten allowlisted keys read-only locally while under management.
//
// Naming: this is a DIFFERENT concept from config.go's existing
// fleetManagedKeys (fleet identity keys -- role/address/master_url/ca_pin/
// node_id -- writable only by `trinetra fleet init|join|leave|disable`).
// Every identifier here is named managedFragment* to keep the two apart.
package trinetra

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// managedFragmentAllowlistKeys is the exact, closed set of config.Config
// keys a managed-config fragment may set: five thresholds,
// the three baseline/anomaly-tuning keys, quiet_hours and
// critical_overrides_quiet. Every one of these is already a live-apply key
// (not RestartRequired -- see config.Keys()/keyCatalog), which is what lets
// the child apply a pushed fragment through the existing ApplyConfig/reload
// path with no restart. Pushing a fallback CHANNEL set is
// explicitly OUT of scope for this fragment mechanism -- deferred to a
// later task.
//
// This delegates to core.ManagedKeys rather than defining
// its own literal copy: internal/web's managed-config page needs this exact
// allowlist too (to build the fragment editor's key <select>), and
// internal/web cannot import internal/trinetra, so the one true copy lives
// in internal/core, which both packages can see.
var managedFragmentAllowlistKeys = core.ManagedKeys

// managedFragmentAllowlist is managedFragmentAllowlistKeys as a set, for O(1)
// membership checks.
var managedFragmentAllowlist = func() map[string]bool {
	m := make(map[string]bool, len(managedFragmentAllowlistKeys))
	for _, k := range managedFragmentAllowlistKeys {
		m[k] = true
	}
	return m
}()

// randomManagedFragmentID returns 48 random bits as 12 hex chars, mirroring
// randomIncidentID/randomSilenceID's shape.
func randomManagedFragmentID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// validateManagedFragmentValues rejects any key outside the allowlist
// (naming it) and validates every remaining value by applying it to a
// scratch copy of config.Default() via config.Set -- the exact same
// validated setter `trinetra config set` uses, so a fragment can never hold
// a value the child itself would ever reject.
func validateManagedFragmentValues(values map[string]string) error {
	for k := range values {
		if !managedFragmentAllowlist[k] {
			return fmt.Errorf("%q is not a managed-config key (allowed: %s)", k, joinManagedFragmentKeys())
		}
	}
	scratch := config.Default()
	// Deterministic order so a test asserting on the first error is stable.
	for _, k := range managedFragmentAllowlistKeys {
		v, ok := values[k]
		if !ok {
			continue
		}
		if err := scratch.Set(k, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

func joinManagedFragmentKeys() string {
	out := ""
	for i, k := range managedFragmentAllowlistKeys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}

// --- master-side fragment store ------------------------------------------

// managedFragmentsFileV1 is fleet/managed.json's on-disk shape.
type managedFragmentsFileV1 struct {
	Fragments  []core.ManagedFragment `json:"fragments"`
	Generation int64                  `json:"generation"`
}

// managedFragmentStore is the master's managed-config fragment store,
// persisted atomically (temp + rename + fsync, via writeFileAtomicSynced) to one
// JSON file, 0600 (global-constraints: fleet/managed.json). generation is a
// single monotonically increasing counter, bumped on every successful
// save/delete: it is both a saved fragment's own Version (task-8 ruling:
// "Version increments on each save") and the desired-set version pushed to
// children in a "managed_config" frame, so a node's push version always
// tells you exactly how fresh its desired set is relative to the store.
type managedFragmentStore struct {
	path string

	mu         sync.Mutex
	fragments  []core.ManagedFragment
	generation int64
}

// loadManagedFragmentStore opens (or creates) the store at path. A missing
// file is not an error: a fresh master has no fragments yet.
func loadManagedFragmentStore(path string) (*managedFragmentStore, error) {
	s := &managedFragmentStore{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var data managedFragmentsFileV1
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("fleet: parse %s: %w", path, err)
	}
	s.fragments, s.generation = data.Fragments, data.Generation
	return s, nil
}

func (s *managedFragmentStore) saveLocked() error {
	b, err := json.MarshalIndent(managedFragmentsFileV1{Fragments: s.fragments, Generation: s.generation}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomicSynced(s.path, b, 0o600)
}

// List returns every fragment, in creation order.
func (s *managedFragmentStore) List() []core.ManagedFragment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.ManagedFragment(nil), s.fragments...)
}

// Version is the store's current generation counter.
func (s *managedFragmentStore) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// Save validates and stores frag: a new fragment (fresh random id) if
// frag.ID is "", otherwise an in-place update of the existing fragment with
// that id. Every key in frag.Values must be allowlisted and itself valid
// (validateManagedFragmentValues); an unknown ID on an update is an error.
// On success frag.Version is set to the store's new generation and
// frag.Author to actor.
func (s *managedFragmentStore) Save(frag core.ManagedFragment, actor string) (core.ManagedFragment, error) {
	if frag.Tag != "" && !fleet.ValidTag(frag.Tag) {
		return core.ManagedFragment{}, fmt.Errorf("invalid tag %q (lowercase letters, digits, _ . -; max 32)", frag.Tag)
	}
	if err := validateManagedFragmentValues(frag.Values); err != nil {
		return core.ManagedFragment{}, err
	}
	frag.Author = actor

	s.mu.Lock()
	defer s.mu.Unlock()

	// Round-1 review MINOR: an empty ID is a server-side UPSERT by tag, not
	// just a create -- "one fragment per tag" is enforced
	// HERE, atomically under this same lock, rather than by a caller
	// (`fleet managed set`) first listing fragments and then deciding
	// whether to create or update: that list-then-write was a TOCTOU (two
	// concurrent `set --tag web ...` calls could both see no existing
	// fragment and both create one, leaving two fragments for the same
	// tag). An explicit, non-empty ID is always a plain update-by-id
	// instead (unchanged behavior).
	explicitID := frag.ID
	if explicitID == "" {
		for _, f := range s.fragments {
			if f.Tag == frag.Tag {
				frag.ID = f.ID
				break
			}
		}
	}

	// Merge carry-over: when the caller asked to merge (`fleet
	// managed set --tag X k=v`'s default), fold frag.Values into the
	// EXISTING fragment's Values -- found by whichever id resolution above
	// landed on -- rather than replacing them wholesale. This runs under
	// the store's own lock, so it's atomic with the write below: no other
	// Save can interleave between reading the old Values and writing the
	// merged result. A brand-new fragment (no existing id) has nothing to
	// merge into, so Merge is a no-op there. frag.Merge itself is cleared
	// so it never round-trips into the persisted fragment.
	if frag.Merge && frag.ID != "" {
		for _, f := range s.fragments {
			if f.ID != frag.ID {
				continue
			}
			merged := make(map[string]string, len(f.Values)+len(frag.Values))
			for k, v := range f.Values {
				merged[k] = v
			}
			for k, v := range frag.Values {
				merged[k] = v
			}
			frag.Values = merged
			break
		}
	}
	frag.Merge = false

	gen := s.generation + 1
	frag.Version = gen

	if frag.ID == "" {
		id, err := randomManagedFragmentID()
		if err != nil {
			return core.ManagedFragment{}, err
		}
		frag.ID = id
		s.fragments = append(s.fragments, frag)
		s.generation = gen
		if err := s.saveLocked(); err != nil {
			s.fragments = s.fragments[:len(s.fragments)-1]
			s.generation--
			return core.ManagedFragment{}, err
		}
		return frag, nil
	}
	for i, f := range s.fragments {
		if f.ID != frag.ID {
			continue
		}
		prev := s.fragments[i]
		s.fragments[i] = frag
		s.generation = gen
		if err := s.saveLocked(); err != nil {
			s.fragments[i] = prev
			s.generation--
			return core.ManagedFragment{}, err
		}
		return frag, nil
	}
	// Only reachable for an explicit, non-empty ID that names nothing (the
	// tag-derived branch above can never produce an ID that isn't already
	// in s.fragments).
	return core.ManagedFragment{}, fmt.Errorf("no such managed-config fragment %q: %w", explicitID, core.ErrNotFound)
}

// Delete removes fragment id, bumping the store's generation.
func (s *managedFragmentStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, f := range s.fragments {
		if f.ID != id {
			continue
		}
		prev := s.fragments
		s.fragments = append(append([]core.ManagedFragment(nil), s.fragments[:i]...), s.fragments[i+1:]...)
		gen := s.generation + 1
		s.generation = gen
		if err := s.saveLocked(); err != nil {
			s.fragments = prev
			s.generation--
			return err
		}
		return nil
	}
	return fmt.Errorf("no such managed-config fragment %q: %w", id, core.ErrNotFound)
}

// ByTag returns the fragment with the given tag ("" for the all-nodes
// fragment), if any -- used by `fleet managed set` to decide whether to
// create a new fragment or update the existing one for that tag (task-8
// ruling: "one fragment per tag, simplest").
func (s *managedFragmentStore) ByTag(tag string) (core.ManagedFragment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.fragments {
		if f.Tag == tag {
			return f, true
		}
	}
	return core.ManagedFragment{}, false
}

// Desired computes the effective (values, key->fragment-id, conflicts) for a
// node carrying nodeTags: the "" (all-nodes) fragment applies first, then
// every fragment whose Tag the node carries, in ALPHABETICAL tag order,
// later values winning key by key. A key set by more than
// one applicable fragment is recorded in conflicts, Fragments listing every
// contributing fragment id in application order (its last entry is the one
// that actually won).
func (s *managedFragmentStore) Desired(nodeTags []string) (values map[string]string, keyFragment map[string]string, conflicts []core.ManagedConflict) {
	s.mu.Lock()
	fragments := append([]core.ManagedFragment(nil), s.fragments...)
	s.mu.Unlock()

	tagSet := make(map[string]bool, len(nodeTags))
	for _, t := range nodeTags {
		tagSet[t] = true
	}
	var allNodes *core.ManagedFragment
	tagFrags := map[string]core.ManagedFragment{}
	for _, f := range fragments {
		f := f
		if f.Tag == "" {
			allNodes = &f
		} else if tagSet[f.Tag] {
			tagFrags[f.Tag] = f
		}
	}
	var applicable []core.ManagedFragment
	if allNodes != nil {
		applicable = append(applicable, *allNodes)
	}
	var tags []string
	for t := range tagFrags {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, t := range tags {
		applicable = append(applicable, tagFrags[t])
	}

	values = map[string]string{}
	keyFragment = map[string]string{}
	contributors := map[string][]string{}
	for _, f := range applicable {
		for _, k := range managedFragmentAllowlistKeys {
			v, ok := f.Values[k]
			if !ok {
				continue
			}
			values[k] = v
			keyFragment[k] = f.ID
			contributors[k] = append(contributors[k], f.ID)
		}
	}
	var keys []string
	for k, ids := range contributors {
		if len(ids) > 1 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		conflicts = append(conflicts, core.ManagedConflict{Key: k, Fragments: contributors[k]})
	}
	return values, keyFragment, conflicts
}

// --- master-side push -----------------------------------------------------

// managedConfigFrameData is the "managed_config" stream frame's Data shape:
// Version is the store's desired-set generation for the node this was built
// for; Values is that node's full desired set; Fragments maps each key in
// Values to the id of the fragment that supplied it (task-8 ruling calls
// for "fragments:[ids]" -- the contributing fragment ids -- which this
// carries PER KEY rather than as a flat list, since both the child's
// read-only refusal message and core.LinkView.Managed need to name exactly
// which fragment owns a given key, not just which fragments exist).
type managedConfigFrameData struct {
	Version   int64             `json:"version"`
	Values    map[string]string `json:"values"`
	Fragments map[string]string `json:"fragments"`
}

// managedPushInterval is how often TickManaged refreshes every connected
// node's pushed managed-config set unconditionally, mirroring
// silencePushInterval's role for pushed silences.
const managedPushInterval = 10 * time.Minute

// managedPusher drives the master's managed_config stream frame to every
// node: on any fragment change (PushToAll, called by fleetAPIImpl after a
// successful Save/Delete), on connect (PushOne, called from hub.OnConnect),
// and unconditionally every managedPushInterval (TickManaged, called from
// masterLoop.tick).
type managedPusher struct {
	push      func(nodeID string, f fleet.Frame) bool
	connected func(nodeID string) bool
	store     *managedFragmentStore
	tagsOf    func(nodeID string) []string

	mu           sync.Mutex
	lastPushedAt int64 // unix time of the last push-to-all pass; 0 = never
}

func newManagedPusher(push func(string, fleet.Frame) bool, connected func(string) bool, store *managedFragmentStore, tagsOf func(string) []string) *managedPusher {
	return &managedPusher{push: push, connected: connected, store: store, tagsOf: tagsOf}
}

// PushOne pushes id's current desired set unconditionally.
func (p *managedPusher) PushOne(id string) bool {
	if p == nil || p.push == nil || p.store == nil {
		return false
	}
	var tags []string
	if p.tagsOf != nil {
		tags = p.tagsOf(id)
	}
	values, keyFragment, _ := p.store.Desired(tags)
	data, err := json.Marshal(managedConfigFrameData{Version: p.store.Version(), Values: values, Fragments: keyFragment})
	if err != nil {
		return false
	}
	return p.push(id, fleet.Frame{Type: "managed_config", Data: data})
}

// PushToAll pushes every connected id in ids its current desired set
// (skipping any not currently connected) -- called after any
// Save/Delete ("on change").
func (p *managedPusher) PushToAll(ids []string) {
	if p == nil {
		return
	}
	for _, id := range ids {
		if p.connected != nil && !p.connected(id) {
			continue
		}
		p.PushOne(id)
	}
}

// TickManaged self-gates to managedPushInterval, mirroring TickSilences's
// periodic-refresh role.
func (p *managedPusher) TickManaged(now time.Time, ids []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	due := p.lastPushedAt == 0 || now.Unix()-p.lastPushedAt >= int64(managedPushInterval/time.Second)
	if due {
		p.lastPushedAt = now.Unix()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	p.PushToAll(ids)
}

// ManagedFragmentFor reports the fragment id currently managing key on THIS
// child, read from its durable sidecar (fleet-child/managed.json) under
// stateDir. This is the exported helper `trinetra config set`/`config
// unset` (cmdConfig, main.go) use: that CLI subcommand is a plain one-shot
// process with no connection to a running daemon's in-memory state, so it
// reads the same durable sidecar managedChild persists to (see
// managedChild.setApplied/clear) -- the one place both a live daemon
// process and this one-shot CLI can agree on what is currently managed.
func ManagedFragmentFor(stateDir, key string) (fragmentID string, managed bool) {
	b, err := os.ReadFile(managedChildPath(stateDir))
	if err != nil {
		return "", false
	}
	var f managedChildFileV1
	if json.Unmarshal(b, &f) != nil {
		return "", false
	}
	if _, ok := f.Values[key]; !ok {
		return "", false
	}
	return f.Fragments[key], true
}

// --- child-side apply/report -----------------------------------------------

// managedChildPath is the sidecar's path under the daemon's state directory:
// <stateDir>/fleet-child/managed.json.
func managedChildPath(stateDir string) string {
	return filepath.Join(fleetChildDir(stateDir), "managed.json")
}

// managedChildFileV1 is the sidecar's on-disk shape. Values/AppliedAt match
// the task-8 ruling verbatim ({version, values, applied_at}); Fragments
// (key -> fragment id) is an additive extension beyond that literal triple,
// needed so a restarted process (and the plain `trinetra config set`
// one-shot CLI, via ManagedFragmentFor above) can still name the owning
// fragment in a read-only refusal before the next push ever arrives --
// documented as a deviation in the task report.
type managedChildFileV1 struct {
	Version   int64             `json:"version"`
	Values    map[string]string `json:"values"`
	Fragments map[string]string `json:"fragments,omitempty"`
	AppliedAt int64             `json:"applied_at"`
}

// managedChild is the child's live state for the master's pushed managed
// config: getCfg/self are the same accessors startChild already has
// (fleetDeps.getCfg, fleetDeps.self); now is injected for tests. version/
// applied/lastErr reflect the LAST APPLY ATTEMPT's outcome (version is only
// advanced on success -- an invalid fragment "rejects the whole fragment:
// keep the old config and report the error", task-8 ruling, so version
// stays at the last GOOD value); fragments is the master's current
// declared key->fragment-id attribution from the last received frame
// (updated regardless of apply success, since it describes what the master
// intends, not whether this child accepted it) -- in-memory only, restored
// best-effort from the sidecar (see loadManagedChild).
//
// values is the set of key/value pairs THIS child currently COMMITS to
// enforcing -- i.e. the last successfully validated managed-config values
// It exists separately from whatever the
// live config happens to hold right now because it is the input to
// reimposeManagedValues, which the daemon's shared full-config reload path
// (daemon.go) calls on EVERY ApplyConfig -- from the channels page, the
// public-settings page, any ctl "manage" screen, none of which know
// anything about managed-config -- to re-force these exact values onto
// whatever config those callers are about to persist, so a stale read (or a
// race with a fresh master push) can never silently revert a managed key.
type managedChild struct {
	path   string
	getCfg func() *config.Config
	self   core.API
	now    func() time.Time

	mu           sync.Mutex
	everReceived bool
	version      int64
	applied      bool
	lastErr      string
	fragments    map[string]string
	values       map[string]string
}

func newManagedChild(path string, getCfg func() *config.Config, self core.API, now func() time.Time) *managedChild {
	if now == nil {
		now = time.Now
	}
	return &managedChild{path: path, getCfg: getCfg, self: self, now: now}
}

// loadManagedChild restores the sidecar written by a previous process: a
// missing or corrupt file just starts fresh (never managed yet), exactly
// like a child that has never received a "managed_config" frame.
func loadManagedChild(path string, getCfg func() *config.Config, self core.API, now func() time.Time) *managedChild {
	mc := newManagedChild(path, getCfg, self, now)
	b, err := os.ReadFile(path)
	if err != nil {
		return mc
	}
	var f managedChildFileV1
	if json.Unmarshal(b, &f) != nil {
		return mc
	}
	mc.everReceived = true
	mc.version = f.Version
	mc.applied = true
	mc.fragments = cloneStringMap(f.Fragments)
	mc.values = cloneStringMap(f.Values)
	return mc
}

// stringMapsEqual reports whether a and b hold the same key/value pairs
// (nil and an empty, non-nil map compare equal).
func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneConfigJSON returns a deep, independent copy of c via a JSON round
// trip (mirrors internal/web's cloneConfig; duplicated here rather than
// shared since internal/web must not be imported by this package and vice
// versa).
func cloneConfigJSON(c *config.Config) (*config.Config, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	nc := &config.Config{}
	if err := json.Unmarshal(b, nc); err != nil {
		return nil, err
	}
	return nc, nil
}

// Apply handles one received "managed_config" frame: an empty Values (every
// fragment deleted) means the child stops enforcing -- local values stay at
// their last managed values, the sidecar is cleared (clear); otherwise every
// value is validated against a clone of the live config via config.Set
// (never the live config directly), and only if EVERY value passes is the
// clone applied via self.ApplyConfig (the existing live-apply/reload path,
// no restart). A single invalid value rejects the whole fragment: the old
// config is left completely untouched and the error is recorded for the
// next report.
//
// Round-1 review, MINOR: an
// incoming frame identical to what is already applied (same version AND
// same values) is a complete no-op -- no re-validation, no ApplyConfig
// call, no sidecar rewrite -- so the periodic (every managedPushInterval)
// re-push does not thrash the disk or the dispatcher on every otherwise-
// unchanged refresh. Round 2 tightened this: version+values matching is no
// longer sufficient on its own -- the LIVE config's effective value for
// every managed key must ALSO already equal what was committed
// (configMatchesValues), or the push re-applies anyway. Without this, a
// child whose live config diverged from its committed values (a direct
// config.json edit, a restored backup, an offline write, or -- before round
// 2 -- a SIGHUP that bypassed reload's reimpose) would short-circuit every
// subsequent same-version push forever: the version/values match on their
// own prove nothing about what the live config currently holds.
//
// Round-1 review, IMPORTANT 1: mc.values (the committed set
// reimposeManagedValues re-forces onto every OTHER full-config apply) is
// updated to the NEW values BEFORE self.ApplyConfig is called, not after --
// self.ApplyConfig ultimately runs the shared reload closure (daemon.go),
// which calls reimposeManagedValues on its way in; committing first means
// THIS push's own new values are what get (redundantly, harmlessly)
// re-imposed onto its own clone, not the stale ones still in mc.values.
// Rolled back to the previous committed values if ApplyConfig itself then
// fails (a downstream reload error unrelated to the values' own validity,
// which already passed above).
func (mc *managedChild) Apply(p managedConfigFrameData) {
	if mc == nil {
		return
	}
	mc.mu.Lock()
	mc.everReceived = true
	mc.fragments = cloneStringMap(p.Fragments)
	sameVersionAndValues := mc.applied && mc.version == p.Version && stringMapsEqual(mc.values, p.Values)
	prevValues := cloneStringMap(mc.values)
	mc.mu.Unlock()
	if sameVersionAndValues && configMatchesValues(mc.getCfg(), p.Values) {
		return
	}

	if len(p.Values) == 0 {
		mc.clear(p.Version)
		return
	}
	cfg := mc.getCfg()
	if cfg == nil {
		mc.setError(fmt.Errorf("no config available"))
		return
	}
	clone, err := cloneConfigJSON(cfg)
	if err != nil {
		mc.setError(err)
		return
	}
	for _, k := range managedFragmentAllowlistKeys {
		v, ok := p.Values[k]
		if !ok {
			continue
		}
		if err := clone.Set(k, v); err != nil {
			mc.setError(fmt.Errorf("%s: %w", k, err))
			return
		}
	}
	// Defensive: a key outside the allowlist should never arrive from a
	// well-behaved master (Save validates it), but a stray one must still
	// reject the whole fragment rather than being silently ignored or
	// (worse) applied through config.Set's own "unknown key" error path
	// under a key config.Set doesn't recognize -- Set already returns an
	// error for any key it doesn't know, so the loop above already covers
	// this for every key actually present in the allowlist range; keys
	// present in p.Values but outside the allowlist are simply never
	// looked at, which is the intended no-op for something a validating
	// master would never send.
	if mc.self == nil {
		mc.setError(fmt.Errorf("no core API available to apply config"))
		return
	}
	mc.mu.Lock()
	mc.values = cloneStringMap(p.Values)
	mc.mu.Unlock()
	if err := mc.self.ApplyConfig(clone); err != nil {
		mc.mu.Lock()
		mc.values = prevValues
		mc.mu.Unlock()
		mc.setError(err)
		return
	}
	mc.setApplied(p.Version, p.Values)
}

// managedChildWriteHook, when set by a test, runs synchronously immediately
// before setApplied's disk write -- used to prove the write happens while
// mc.mu is still held, the same way
// pushedSilencesWriteHook proves it for pushedSilences.Set.
var managedChildWriteHook func()

// setApplied holds mc.mu for the entire call, including the disk write:
// otherwise two concurrent Apply calls (a stacked "managed_config" push
// arriving before the previous one's write lands) can complete their writes
// out of order, leaving the sidecar stale relative to mc's in-memory state.
func (mc *managedChild) setApplied(version int64, values map[string]string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	mc.version, mc.applied, mc.lastErr = version, true, ""
	fragments := cloneStringMap(mc.fragments)
	b, err := json.Marshal(managedChildFileV1{Version: version, Values: values, Fragments: fragments, AppliedAt: mc.now().Unix()})
	if err != nil {
		return
	}
	if managedChildWriteHook != nil {
		managedChildWriteHook()
	}
	_ = writeFileAtomicSynced(mc.path, b, 0o600)
}

func (mc *managedChild) setError(err error) {
	mc.mu.Lock()
	mc.applied, mc.lastErr = false, err.Error()
	mc.mu.Unlock()
}

// clear resets to "nothing managed" (every fragment deleted on the master)
// and removes the sidecar. Local config values are left exactly as they
// are -- they simply stop being enforced/read-only.
func (mc *managedChild) clear(version int64) {
	mc.mu.Lock()
	mc.version, mc.applied, mc.lastErr, mc.fragments, mc.values = version, true, "", map[string]string{}, map[string]string{}
	mc.mu.Unlock()
	_ = os.Remove(mc.path)
}

// FragmentsSnapshot returns a copy of the current key->fragment-id
// attribution, for core.LinkView.Managed (fleetAPIImpl.Status).
func (mc *managedChild) FragmentsSnapshot() map[string]string {
	if mc == nil {
		return nil
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return cloneStringMap(mc.fragments)
}

// CurrentValues returns a copy of the managed-config values this child
// currently commits to enforcing (nil if mc is nil or nothing is currently
// managed), for reimposeManagedValues. Nil-safe.
func (mc *managedChild) CurrentValues() map[string]string {
	if mc == nil {
		return nil
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return cloneStringMap(mc.values)
}

// reimposeManagedValues re-forces every one of mc's currently-committed
// managed-config values onto c via config.Set.
// The daemon's ONE shared full-config apply path -- the reload closure in
// daemon.go, which inprocAPI.ApplyConfig calls for every control-socket
// ApplyConfig (the web channels page, the public-settings page, every ctl
// "manage" screen) -- calls this on every incoming config, right before
// persisting/applying it: none of those callers know anything about
// managed-config, so without this a full-config round trip built from a
// stale read (or racing a fresh master push) could silently revert a
// managed key back to whatever value it happened to carry.
//
// mc nil (solo, master, or before a child's managedChild is wired at daemon
// startup) or nothing currently managed is a complete no-op. A Set failure
// here should not happen (these values were already validated once by
// managedChild.Apply), but is deliberately swallowed rather than failing
// the whole apply: an unrelated, unmanaged edit (e.g. a channel change)
// must never be rejected outright because of it.
func reimposeManagedValues(mc *managedChild, c *config.Config) {
	if c == nil {
		return
	}
	values := mc.CurrentValues()
	for _, k := range managedFragmentAllowlistKeys {
		v, ok := values[k]
		if !ok {
			continue
		}
		_ = c.Set(k, v)
	}
}

// configMatchesValues reports whether c's current effective value for every
// key in values matches it exactly: used both to
// tighten managedChild.Apply's short-circuit (see its doc comment) and by
// reconcileManagedValuesAtStart to decide whether a startup reconciliation
// write is actually needed. A nil c never matches (vacuously "diverged",
// forcing a caller to treat it as needing correction rather than silently
// trusting an absent config).
func configMatchesValues(c *config.Config, values map[string]string) bool {
	if c == nil {
		return false
	}
	for k, v := range values {
		cur, ok := c.Get(k)
		if !ok || cur != v {
			return false
		}
	}
	return true
}

// reconcileManagedValuesAtStart re-imposes mc's committed managed values (if
// any) onto the live config immediately at child startup, persisting the
// correction via the normal reload path if the live config had actually
// diverged: a child's config can drift from its
// committed values between restarts -- a direct config.json edit, a
// restored backup, an offline write, or a SIGHUP that
// bypassed reload's reimpose -- and, combined with Apply's short-circuit,
// that divergence would otherwise be permanent: every subsequent push at
// the SAME version would short-circuit forever without this reconciliation
// ever running. Called once, from startChild, right after loadManagedChild
// and before the shipper starts.
//
// A nil mc, nothing currently managed, or a live config that already
// matches is a complete no-op (no clone, no ApplyConfig call). Errors are
// logged, never returned or panicked on: this must never prevent the child
// from starting.
func reconcileManagedValuesAtStart(mc *managedChild, logf func(string, ...any)) {
	if mc == nil {
		return
	}
	values := mc.CurrentValues()
	if len(values) == 0 {
		return
	}
	cfg := mc.getCfg()
	if configMatchesValues(cfg, values) {
		return
	}
	clone, err := cloneConfigJSON(cfg)
	if err != nil {
		if logf != nil {
			logf("fleet: managed config: could not check for drift at startup: %v", err)
		}
		return
	}
	reimposeManagedValues(mc, clone)
	if mc.self == nil {
		return
	}
	if err := mc.self.ApplyConfig(clone); err != nil {
		if logf != nil {
			logf("fleet: managed config: could not persist reconciled values at startup: %v", err)
		}
	}
}

// Report builds this child's fleet.ManagedReport for its next LiveUpdate:
// nil if no "managed_config" frame has ever been received (an old master,
// or simply not yet connected to a phase-2 one) -- leaves the child
// completely unaffected, matching every other task-8 mechanism's
// nil-is-a-no-op convention. Values are read fresh from the live config on
// every call, never cached, so the master always sees this child's true
// current effective values regardless of whether the last apply succeeded.
func (mc *managedChild) Report() *fleet.ManagedReport {
	if mc == nil {
		return nil
	}
	mc.mu.Lock()
	everReceived, version, applied, lastErr := mc.everReceived, mc.version, mc.applied, mc.lastErr
	mc.mu.Unlock()
	if !everReceived {
		return nil
	}
	values := map[string]string{}
	if cfg := mc.getCfg(); cfg != nil {
		for _, k := range managedFragmentAllowlistKeys {
			if v, ok := cfg.Get(k); ok {
				values[k] = v
			}
		}
	}
	return &fleet.ManagedReport{Version: version, Applied: applied, Error: lastErr, Values: values}
}

// applyManagedConfigFrame decodes and applies f if it is a "managed_config"
// frame, ignoring anything else -- called from startChild's OnFrame
// closure alongside onStreamFrame/applyAckFrame. A malformed payload is
// dropped rather than panicking, exactly like onStreamFrame's other frame
// types: this runs synchronously on the stream's read loop and must never
// block or crash on hostile/garbled input from the wire.
func applyManagedConfigFrame(mc *managedChild, f fleet.Frame) {
	if mc == nil || f.Type != "managed_config" {
		return
	}
	var p managedConfigFrameData
	if json.Unmarshal(f.Data, &p) != nil {
		return
	}
	mc.Apply(p)
}
