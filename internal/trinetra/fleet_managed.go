// Package trinetra: fleet_managed.go is the master's managed-config fragment store and push
// plus the child's counterpart that applies whatever the master last pushed, live.
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

// managedFragmentAllowlistKeys is the exact, closed set of config.Config keys a
// managed-config fragment may set: five thresholds, the three baseline/anomaly-tuning keys.
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

// validateManagedFragmentValues rejects any key outside the allowlist.
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

// managedFragmentStore is the master's managed-config fragment store, persisted atomically
// (temp + rename + fsync, via writeFileAtomicSynced) to one 0600 JSON file.
type managedFragmentStore struct {
	path string

	mu         sync.Mutex
	fragments  []core.ManagedFragment
	generation int64
}

// loadManagedFragmentStore opens (or creates) the store at path.
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

// Save validates and stores frag: a new fragment (fresh random id) if frag.ID is "",
// otherwise an in-place update of the existing fragment with that id.
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

	// An empty ID is a server-side UPSERT by tag, not just a create: "one fragment per tag" is
	// enforced HERE, atomically under this lock.
	explicitID := frag.ID
	if explicitID == "" {
		for _, f := range s.fragments {
			if f.Tag == frag.Tag {
				frag.ID = f.ID
				break
			}
		}
	}

	// Merge carry-over: when the caller asked to merge (`fleet managed set --tag X k=v`'s
	// default), fold frag.Values into the EXISTING fragment's Values.
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
	// Only reachable for an explicit, non-empty ID that names nothing (the tag-derived branch
	// above can never produce an ID that isn't already in s.fragments).
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

// ByTag returns the fragment with the given tag ("" for the all-nodes fragment), if any.
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

// Desired computes the effective (values, key->fragment-id, conflicts) for a node carrying
// nodeTags: the "" (all-nodes) fragment applies first.
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

// managedConfigFrameData is the "managed_config" stream frame's Data shape.
type managedConfigFrameData struct {
	Version   int64             `json:"version"`
	Values    map[string]string `json:"values"`
	Fragments map[string]string `json:"fragments"`
}

// managedPushInterval is how often TickManaged refreshes every connected node's pushed
// managed-config set unconditionally.
const managedPushInterval = 10 * time.Minute

// managedPusher drives the master's managed_config stream frame to every node: on any
// fragment change (PushToAll, called by fleetAPIImpl after a successful Save/Delete).
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

// PushToAll pushes every connected id in ids its current desired set (skipping any not
// currently connected) -- called after any Save/Delete ("on change").
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

// ManagedFragmentFor reports the fragment id currently managing key on THIS child, read
// from its durable sidecar (fleet-child/managed.json) under stateDir.
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

// managedChildFileV1 is the sidecar's on-disk shape: {version, values, applied_at} plus
// Fragments (key -> fragment id), which lets a restarted process.
type managedChildFileV1 struct {
	Version   int64             `json:"version"`
	Values    map[string]string `json:"values"`
	Fragments map[string]string `json:"fragments,omitempty"`
	AppliedAt int64             `json:"applied_at"`
}

// managedChild is the child's live state for the master's pushed managed config:
// getCfg/self are startChild's accessors (fleetDeps.getCfg, fleetDeps.self).
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

// loadManagedChild restores the sidecar written by a previous process: a missing or corrupt
// file just starts fresh (never managed yet).
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

// cloneConfigJSON returns a deep, independent copy of c via a JSON round trip.
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

// Apply handles one received "managed_config" frame: an empty Values (every fragment
// deleted) means the child stops enforcing.
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
	// Defensive: a key outside the allowlist should never arrive from a well-behaved master
	// (Save validates it).
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

// managedChildWriteHook, when set by a test, runs synchronously right before setApplied's
// disk write, to prove the write happens while mc.mu is held.
var managedChildWriteHook func()

// setApplied holds mc.mu for the entire call, including the disk write: otherwise two
// concurrent Apply calls.
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

// clear resets to "nothing managed" (every fragment deleted on the master) and removes the
// sidecar.
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

// CurrentValues returns a copy of the managed-config values this child currently commits to
// enforcing (nil if mc is nil or nothing is currently managed), for reimposeManagedValues.
func (mc *managedChild) CurrentValues() map[string]string {
	if mc == nil {
		return nil
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return cloneStringMap(mc.values)
}

// reimposeManagedValues re-forces every one of mc's currently-committed managed-config
// values onto c via config.Set.
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

// configMatchesValues reports whether c's current effective value for every key in values
// matches it exactly: used both to tighten managedChild.Apply's short-circuit.
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

// reconcileManagedValuesAtStart re-imposes mc's committed managed values (if any) onto the
// live config immediately at child startup.
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

// Report builds this child's fleet.ManagedReport for its next LiveUpdate: nil if no
// "managed_config" frame has ever been received.
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

// applyManagedConfigFrame decodes and applies f if it is a "managed_config" frame, ignoring
// anything else.
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
