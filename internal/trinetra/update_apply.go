package trinetra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// errUpdateInProgress is returned when another apply/rollback/install holds
// update/apply.lock, or a Pending update is already recorded: a second
// `trinetra update apply` (CLI, socket or web, or a concurrent one) must not
// stage a new build over a host that has not yet confirmed the last one.
var errUpdateInProgress = errors.New("update: an update is already in progress (see trinetra update status)")

// takeApplyLock claims update/apply.lock without blocking: one apply,
// rollback or install at a time per host, across processes. A held lock is
// errUpdateInProgress. The kernel drops the lock if the holder dies.
func takeApplyLock(p updatePaths) (unlock func(), err error) {
	unlock, ok, err := update.TryLock(p.applyLock())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errUpdateInProgress
	}
	return unlock, nil
}

// probeApplyLock reports errUpdateInProgress when another operation holds
// the apply lock right now, without keeping it (the socket preflights use
// it to refuse fast; the operation itself takes the lock for real).
func probeApplyLock(p updatePaths) error {
	unlock, err := takeApplyLock(p)
	if err != nil {
		return err
	}
	unlock()
	return nil
}

// updateHealthDeadline is how long a freshly-swapped-in build has to prove
// itself healthy (see the Pending.Deadline this package sets) before
// whatever watches it rolls back.
const updateHealthDeadline = 90 * time.Second

// healthDeadline is the health window every Pending and guard uses:
// updateHealthDeadline, except in a trinetra_testkeys build whose
// update-e2e harness shortens it via TRINETRA_E2E_HEALTH_DEADLINE
// (update_e2e_hooks_testkeys.go). A default build never reads that
// variable (update_e2e_hooks.go).
func healthDeadline() time.Duration {
	if d, ok := e2eHealthDeadline(); ok {
		return d
	}
	return updateHealthDeadline
}

// updatePaths locates everything a self-update touches: the binaries in
// BinDir, this package's scratch space under StateDir/update (staged
// downloads, the previous build kept for rollback, cached manifests, state
// and locks), the pinned guard binary in GuardDir, and the watchdog units in
// UnitDir.
type updatePaths struct{ BinDir, StateDir, GuardDir, UnitDir string }

// defaultGuardDir holds the pinned guard binary: a copy of the binary
// that performed the last apply (or install), on an exec-friendly root path
// (not /var/lib, which is often noexec or labelled non-executable), so the
// guard and the watchdog never execute the new, unproven build.
const defaultGuardDir = "/usr/local/lib/trinetra/guard"

// defaultUpdatePaths is what production callers use: the real install
// location and the package-level stateDir (itself overridable in tests).
func defaultUpdatePaths() updatePaths {
	return updatePaths{BinDir: "/usr/local/bin", StateDir: stateDir, GuardDir: defaultGuardDir, UnitDir: "/etc/systemd/system"}
}

func (p updatePaths) dir() string             { return filepath.Join(p.StateDir, "update") }
func (p updatePaths) staging(v string) string { return filepath.Join(p.dir(), "staging", v) }
func (p updatePaths) previous() string        { return filepath.Join(p.dir(), "previous") }
func (p updatePaths) cache() string           { return filepath.Join(p.dir(), "cache") }
func (p updatePaths) applyLock() string       { return filepath.Join(p.dir(), "apply.lock") }
func (p updatePaths) guardLock() string       { return filepath.Join(p.dir(), "guard.lock") }
func (p updatePaths) guardBin() string        { return filepath.Join(p.GuardDir, "trinetra") }

// Pending phases: swapIn records Pending in phase pendingSwapping
// before its first rename and moves it to pendingSwapped once every binary
// and plugins.json are in place. A guard that finds pendingSwapping with no
// live apply holding apply.lock knows the swap was interrupted. A Pending
// with no phase (written before phases existed) counts as swapped.
const (
	pendingSwapping = "swapping"
	pendingSwapped  = "swapped"
)

// selfExecutable is the file the pinned guard is copied from: the running
// binary. /proc/self/exe is preferred on Linux because it still opens after
// the file on disk has been replaced (a daemon serving a socket apply after
// an install); os.Executable would then name a "(deleted)" path. A variable
// so tests can point it at a small fixture.
var selfExecutable = func() (string, error) {
	if _, err := os.Stat("/proc/self/exe"); err == nil {
		return "/proc/self/exe", nil
	}
	return os.Executable()
}

// writePinnedGuard copies the running binary to p.guardBin() (dir and file
// 0755, same-directory temp file, fsync, rename, directory fsync): the code
// that writes a Pending is the code that resolves it.
func writePinnedGuard(p updatePaths) error {
	if p.GuardDir == "" {
		return errors.New("update: no guard directory configured")
	}
	self, err := selfExecutable()
	if err != nil {
		return fmt.Errorf("update: locate the running binary: %w", err)
	}
	if err := os.MkdirAll(p.GuardDir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(p.GuardDir, 0o755); err != nil {
		return err
	}
	// A freshly created guard dir is only durable once its parent is synced.
	if err := syncDirFn(filepath.Dir(p.GuardDir)); err != nil {
		return err
	}
	if err := copyFile(self, p.guardBin(), 0o755); err != nil {
		return fmt.Errorf("update: write the pinned guard binary: %w", err)
	}
	return nil
}

var syncDirFn = syncDir

// runProbe runs bin with args, and if the exec is refused (a noexec mount or
// SELinux label on /var/lib) retries from a temporary copy beside the guard
// dir, which install already requires to be executable (#141).
func runProbe(p updatePaths, x Exec, bin string, args ...string) ([]byte, error) {
	out, err := x.Run(bin, args...)
	if err == nil || !errors.Is(err, fs.ErrPermission) || p.GuardDir == "" {
		return out, err
	}
	dir := filepath.Dir(p.GuardDir)
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return out, err
	}
	f, cErr := os.CreateTemp(dir, ".probe-")
	if cErr != nil {
		return out, err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)
	if cErr := copyFile(bin, tmp, 0o700); cErr != nil {
		return out, err
	}
	return x.Run(tmp, args...)
}

type probeExec struct {
	p updatePaths
	x Exec
}

func (e probeExec) Run(name string, args ...string) ([]byte, error) {
	return runProbe(e.p, e.x, name, args...)
}

// applyPlan is what a release manifest resolves to for THIS host: the
// manifest itself (plus its raw bytes, for caching), the arch to install
// for, and the subset of Files/Names to fetch and swap in. Files[i] is the
// release asset for the installed binary Names[i].
type applyPlan struct {
	Manifest update.Manifest
	Raw      []byte
	Arch     string
	Files    []update.File
	Names    []string
}

// updateBinaries lists every binary self-update knows how to swap, in the
// fixed order planApply/swapIn/restorePrevious always process them:
// "trinetra" (the core daemon, always installed and always required) then
// each companion plugin recognised by pluginManifestNames.
var updateBinaries = append([]string{"trinetra"}, pluginNamesWithPrefix()...)

// pluginNamesWithPrefix turns pluginManifestNames ("ctl", "web", from
// systemd.go) into their installed binary names ("trinetra-ctl",
// "trinetra-web"), so updateBinaries and pluginManifestNames can never drift
// apart.
func pluginNamesWithPrefix() []string {
	names := make([]string, len(pluginManifestNames))
	for i, n := range pluginManifestNames {
		names[i] = "trinetra-" + n
	}
	return names
}

// planApply picks, for arch, the release asset for every binary that is
// either the core ("trinetra", always required) or reported installed by
// installed(name). A release missing an asset for an installed binary is an
// error: partial upgrades (core newer than a plugin it dials) are exactly
// what version stamping (#107) exists to catch, so self-update must not
// silently leave one behind.
func planApply(m update.Manifest, raw []byte, arch string, installed func(name string) bool) (applyPlan, error) {
	plan := applyPlan{Manifest: m, Raw: raw, Arch: arch}
	for _, name := range updateBinaries {
		if name != "trinetra" && !installed(name) {
			continue
		}
		want := name + "-linux-" + arch
		found := false
		for _, f := range m.Files {
			if f.Name == want && f.Arch == arch {
				plan.Files = append(plan.Files, f)
				plan.Names = append(plan.Names, name)
				found = true
				break
			}
		}
		if !found {
			if name == "trinetra" {
				return applyPlan{}, fmt.Errorf("update: release %s has no %s", m.Version, want)
			}
			return applyPlan{}, fmt.Errorf("update: release %s has no %s, but %s is installed", m.Version, want, name)
		}
	}
	return plan, nil
}

// stage fetches (and hash-verifies, via update.FetchVerified) every file in
// plan into StateDir/update/staging/<version>/, replacing any earlier
// staging attempt for the same version. It also caches the manifest bytes
// (plan.Raw), when present, so `trinetra update status` can show what was
// staged without re-fetching it.
func stage(ctx context.Context, p updatePaths, src update.Source, plan applyPlan) error {
	dir := p.staging(plan.Manifest.Version)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range plan.Files {
		if _, err := update.FetchVerified(ctx, src, plan.Manifest.Version, f, dir, 0o755); err != nil {
			return err
		}
	}
	if len(plan.Raw) > 0 {
		if err := os.MkdirAll(p.cache(), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(p.cache(), "manifest-"+plan.Manifest.Version+".json"), plan.Raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// smokeTestTimeout bounds how long the freshly staged core binary gets to
// answer `version --json` in smokeTest. It is deliberately far tighter than
// the shared execTimeout collectors get for legitimately slow host commands:
// a binary that cannot report its own version within this window is not
// healthy enough to trust with a swap. Callers pass timeoutExec{smokeTestTimeout}.
const smokeTestTimeout = 5 * time.Second

// timeoutExec is an Exec bound by a fixed timeout of its own rather than the
// package-level execTimeout osExec uses -- so a caller like smokeTest is not
// stuck sharing the 60s budget a slow df/docker/smartctl legitimately needs.
type timeoutExec struct{ d time.Duration }

func (t timeoutExec) Run(name string, args ...string) ([]byte, error) {
	return runWithTimeout(t.d, name, args...)
}

// smokeTest runs the freshly staged core binary (`path version --json`) and
// checks it reports the version we just staged, before it is ever trusted to
// run as the daemon. x is expected to be timeoutExec{smokeTestTimeout} in
// production; smokeTest itself applies no timeout of
// its own, so the bound comes entirely from x.
func smokeTest(x Exec, path, want string) error {
	out, err := x.Run(path, "version", "--json")
	if err != nil {
		return fmt.Errorf("update: new binary did not run: %w", err)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return fmt.Errorf("update: new binary version output: %w", err)
	}
	if strings.TrimPrefix(v.Version, "v") != strings.TrimPrefix(want, "v") {
		return fmt.Errorf("update: new binary reports %s, manifest says %s", v.Version, want)
	}
	return nil
}

// replaceFile copies src onto dst atomically (copyFile writes a same-dir
// temp file then renames it into place), so dst is only ever the complete
// old file or the complete new one -- never a partial write, even if src is
// a currently-running executable.
func replaceFile(src, dst string) error {
	return copyFile(src, dst, 0o755)
}

// replaceFileFn is what swapIn replaces each binary with; a variable only so
// tests can observe the state at, or crash, a given rename.
var replaceFileFn = replaceFile

// pluginManifestBase is the bare file name copyFile/restorePrevious use for
// the plugin checksum manifest inside previous/, matching
// filepath.Base(pluginManifestPath()).
func pluginManifestBase() string { return filepath.Base(pluginManifestPath()) }

// swapIn is the only step that touches BinDir. The caller holds
// update/apply.lock. Order matters for crash safety: everything a
// recovery needs is durable before the first rename, so a crash at any
// point after that is resolved by the update watchdog's guard.
//
//  1. Refuse if an update is already Pending, or if there is no installed
//     core binary to keep for rollback.
//  2. Snapshot the current binaries (and plugins.json, if any) into
//     previous.new/, fsync, then swap it into place as previous/.
//  3. Write the pinned guard binary (a copy of this binary).
//  4. Re-hash every staged file against the manifest.
//  5. Record Pending{phase: swapping, version, from, files} (fsynced).
//  6. Atomically replace each installed binary with its staged file.
//  7. Rewrite plugins.json for the new binaries.
//  8. Move Pending to phase swapped, with a fresh health deadline.
//
// A failure in steps 6-7 restores the previous build and clears Pending; if
// that restore also fails, the error joins both and Pending is kept so the
// watchdog finishes the job.
func swapIn(p updatePaths, plan applyPlan, now time.Time) error {
	st, err := update.LoadState(p.dir())
	if err != nil {
		return err
	}
	if st.Pending != nil {
		return errUpdateInProgress
	}
	if fi, err := os.Lstat(filepath.Join(p.BinDir, "trinetra")); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("update: %s is missing or not a regular file; nothing to keep for rollback (reinstall with trinetra install)", filepath.Join(p.BinDir, "trinetra"))
	}

	if err := snapshotPrevious(p, plan.Names); err != nil {
		return fmt.Errorf("update: snapshot the current build: %w", err)
	}
	if err := writePinnedGuard(p); err != nil {
		return err
	}

	stagingDir := p.staging(plan.Manifest.Version)
	for _, f := range plan.Files {
		got, err := update.HashFile(filepath.Join(stagingDir, f.Name))
		if err != nil {
			return fmt.Errorf("update: staged %s: %w", f.Name, err)
		}
		if got != f.SHA256 {
			return fmt.Errorf("update: staged %s changed after verification (sha256 %s, manifest says %s)", f.Name, got, f.SHA256)
		}
	}

	pending := update.Pending{
		Version:  plan.Manifest.Version,
		From:     strings.TrimPrefix(version.String(), "v"),
		Deadline: now.Add(healthDeadline()).Unix(),
		Files:    plan.Names,
		Phase:    pendingSwapping,
	}
	if err := setPending(p, &pending); err != nil {
		return err
	}

	for i, f := range plan.Files {
		dst := filepath.Join(p.BinDir, plan.Names[i])
		if err := replaceFileFn(filepath.Join(stagingDir, f.Name), dst); err != nil {
			return restoreAfterFailedSwap(p, fmt.Errorf("update: install %s: %w", plan.Names[i], err))
		}
		if i == 0 {
			e2eAfterFirstRename()
		}
	}

	if err := writePluginManifest(p.BinDir); err != nil {
		return restoreAfterFailedSwap(p, fmt.Errorf("update: rewrite plugin manifest: %w", err))
	}

	pending.Phase = pendingSwapped
	pending.Deadline = now.Add(healthDeadline()).Unix()
	return setPending(p, &pending)
}

// setPending records (or, with nil, clears) Pending under the state lock.
func setPending(p updatePaths, pending *update.Pending) error {
	return update.WithState(p.dir(), func(st *update.State) error {
		st.Pending = pending
		return nil
	})
}

// snapshotPrevious copies the named binaries (those present) and
// plugins.json into previous.new/, fsyncs it, and only then replaces
// previous/ with it, so a failure or crash part-way never destroys the last
// complete rollback copy.
func snapshotPrevious(p updatePaths, names []string) error {
	prev := p.previous()
	next := prev + ".new"
	if err := os.RemoveAll(next); err != nil {
		return err
	}
	if err := os.MkdirAll(next, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		src := filepath.Join(p.BinDir, name)
		if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := copyFile(src, filepath.Join(next, name), 0o755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(pluginManifestPath()); err == nil {
		if err := copyFile(pluginManifestPath(), filepath.Join(next, pluginManifestBase()), 0o600); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(prev); err != nil {
		return err
	}
	if err := os.Rename(next, prev); err != nil {
		return err
	}
	return update.SyncDir(p.dir())
}

// restoreAfterFailedSwap is swapIn's failure path once renames may have
// started: restore the previous build and clear Pending. If the restore
// fails too, Pending is kept (phase swapping) so the watchdog's guard
// retries it, and the error says so.
func restoreAfterFailedSwap(p updatePaths, cause error) error {
	if rerr := restorePrevious(p); rerr != nil {
		return errors.Join(cause, fmt.Errorf("update: restoring the previous build also failed (host may be half-updated; the update watchdog will retry within a minute): %w", rerr))
	}
	if err := setPending(p, nil); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// restorePrevious puts back everything swapIn snapshotted into previous/:
// each binary via the same atomic same-dir-temp-then-rename copyFile uses,
// and plugins.json verbatim (it describes the binaries being restored, not
// the ones swapIn just failed to fully install). It attempts every file and
// returns the first error, so one unreadable entry does not stop the rest of
// the rollback.
func restorePrevious(p updatePaths) error {
	entries, err := os.ReadDir(p.previous())
	if err != nil {
		return err
	}
	manifestName := pluginManifestBase()
	var firstErr error
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		src := filepath.Join(p.previous(), e.Name())
		var rerr error
		if e.Name() == manifestName {
			rerr = copyFile(src, pluginManifestPath(), 0o600)
		} else {
			rerr = replaceFile(src, filepath.Join(p.BinDir, e.Name()))
		}
		if rerr != nil && firstErr == nil {
			firstErr = rerr
		}
	}
	return firstErr
}
