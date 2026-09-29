package trinetra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// errUpdateInProgress is returned by swapIn when a Pending update is already
// recorded: a second `trinetra update apply` (or a concurrent one) must not
// stage a new build over a host that has not yet confirmed the last one.
var errUpdateInProgress = errors.New("update: another update is in progress (see trinetra update status)")

// updateHealthDeadline is how long a freshly-swapped-in build has to prove
// itself healthy (see the Pending.Deadline this package sets) before
// whatever watches it rolls back.
const updateHealthDeadline = 90 * time.Second

// updatePaths locates everything a self-update touches: the binaries in
// BinDir, and this package's scratch space under StateDir/update (staged
// downloads, the previous build kept for rollback, and cached manifest/sigs).
type updatePaths struct{ BinDir, StateDir string }

// defaultUpdatePaths is what production callers use: the real install
// location and the package-level stateDir (itself overridable in tests).
func defaultUpdatePaths() updatePaths {
	return updatePaths{BinDir: "/usr/local/bin", StateDir: stateDir}
}

func (p updatePaths) dir() string             { return filepath.Join(p.StateDir, "update") }
func (p updatePaths) staging(v string) string { return filepath.Join(p.dir(), "staging", v) }
func (p updatePaths) previous() string        { return filepath.Join(p.dir(), "previous") }
func (p updatePaths) cache() string           { return filepath.Join(p.dir(), "cache") }

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

// smokeTest runs the freshly staged core binary (`path version --json`) and
// checks it reports the version we just staged, before it is ever trusted to
// run as the daemon. The subprocess timeout is bounded by Exec's own
// execTimeout (osExec's real implementation), since the Exec interface takes
// no per-call deadline; smokeTest itself adds no extra bound.
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

// pluginManifestBase is the bare file name copyFile/restorePrevious use for
// the plugin checksum manifest inside previous/, matching
// filepath.Base(pluginManifestPath()).
func pluginManifestBase() string { return filepath.Base(pluginManifestPath()) }

// swapIn is the only step that touches BinDir. Nothing on disk changes
// before every staged file re-hashes to its manifest SHA-256 (step 3 below);
// any failure at or after the first rename (steps 4-5) restores the previous
// build before returning, so a host is never left half-upgraded.
//
//  1. Refuse if an update is already Pending.
//  2. Snapshot the current binaries (and plugins.json, if any) into
//     previous/, replacing whatever was kept from an earlier update.
//  3. Re-hash every staged file against the manifest.
//  4. Atomically replace each installed binary with its staged file.
//  5. Rewrite plugins.json for the new binaries.
//  6. Record a Pending marker so a health check can confirm or roll back.
func swapIn(p updatePaths, plan applyPlan, now time.Time) error {
	st, err := update.LoadState(p.dir())
	if err != nil {
		return err
	}
	if st.Pending != nil {
		return errUpdateInProgress
	}

	prev := p.previous()
	if err := os.RemoveAll(prev); err != nil {
		return err
	}
	if err := os.MkdirAll(prev, 0o700); err != nil {
		return err
	}
	for _, name := range plan.Names {
		src := filepath.Join(p.BinDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := copyFile(src, filepath.Join(prev, name), 0o755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(pluginManifestPath()); err == nil {
		if err := copyFile(pluginManifestPath(), filepath.Join(prev, pluginManifestBase()), 0o600); err != nil {
			return err
		}
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

	for i, f := range plan.Files {
		dst := filepath.Join(p.BinDir, plan.Names[i])
		if err := replaceFile(filepath.Join(stagingDir, f.Name), dst); err != nil {
			_ = restorePrevious(p)
			return err
		}
	}

	if err := writePluginManifest(p.BinDir); err != nil {
		_ = restorePrevious(p)
		return err
	}

	st.Pending = &update.Pending{
		Version:  plan.Manifest.Version,
		From:     strings.TrimPrefix(version.String(), "v"),
		Deadline: now.Add(updateHealthDeadline).Unix(),
		Files:    plan.Names,
	}
	return update.SaveState(p.dir(), st)
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
