package trinetra

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Migration of a pre-rename serverwatch install to trinetra, run by
// `trinetra install` before the normal install (see cmdInstall and the
// "Migration" section of the rename spec).
//
// Safety model, since this runs as root against live data:
//
//   - planLegacyMigration only looks; it never changes anything. When both a
//     serverwatch and a trinetra install exist it refuses (never merges).
//   - Before any data moves, an in-progress marker (migratingMarker) is written
//     into each legacy dir. It travels with the data (rename or copy), so a
//     re-run can tell "a trinetra dir this migration produced" from "a trinetra
//     install someone else made", and resume instead of refusing.
//   - A directory moves by rename(2) (atomic). On EXDEV it is copied into a
//     staging sibling of the destination, the copy is verified entry by entry
//     (type, mode, owner, size, symlink target, SHA-256 of content), the
//     staging dir is renamed into place (atomic), and only then is the source
//     deleted, marker last, so a partly-deleted source is still recognisable.
//   - Every step is safe to re-run after a crash at any point; the in-progress
//     markers are removed only as the very last step.
//   - On any error the migration stops (no automatic rollback that could
//     itself fail half-way) and reports the exact state plus how to finish or
//     roll back by hand.

// Legacy (pre-rename) and new install paths, without any root prefix.
const (
	legacyConfigDirPath = "/etc/serverwatch"
	legacyStateDirPath  = "/var/lib/serverwatch"
	legacyUnitFilePath  = "/etc/systemd/system/serverwatch.service"
	legacyBinFilePath   = "/usr/local/bin/serverwatch"
	legacyCtlFilePath   = "/usr/local/bin/serverwatch-ctl"
	legacyWebFilePath   = "/usr/local/bin/serverwatch-web"
	legacyUsrBinPath    = "/usr/bin/serverwatch"
	legacyServiceName   = "serverwatch"

	newConfigDirPath = "/etc/trinetra"
	newBinFilePath   = "/usr/local/bin/trinetra"

	// migratingMarker is written into each legacy dir before it moves and
	// removed from the new dirs once the whole migration has finished.
	migratingMarker = ".migrating-from-serverwatch"
	// migratedFromServerwatchMarker (in the new state dir) records that, and when, this
	// host was migrated.
	migratedFromServerwatchMarker = "migrated-from-serverwatch"
	// stagingSuffix names the sibling of a new dir that an EXDEV copy is
	// written into before being renamed into place.
	stagingSuffix = ".migrating"
)

// legacyRoot prefixes every migration path; tests point it at a temp dir.
var legacyRoot = ""

// migrationPaths holds every absolute path the migration touches, so tests
// can run the whole thing under a temp root.
type migrationPaths struct {
	OldConfigDir, OldStateDir, OldUnit, OldDropIn string
	OldBin, OldCtl, OldWeb, OldUsrBin             string
	NewConfigDir, NewStateDir, NewBin             string
}

func migrationPathsAt(root string) migrationPaths {
	j := func(p string) string { return filepath.Join(root, p) }
	if root == "" {
		j = func(p string) string { return p }
	}
	return migrationPaths{
		OldConfigDir: j(legacyConfigDirPath),
		OldStateDir:  j(legacyStateDirPath),
		OldUnit:      j(legacyUnitFilePath),
		OldDropIn:    j(legacyUnitFilePath + ".d"),
		OldBin:       j(legacyBinFilePath),
		OldCtl:       j(legacyCtlFilePath),
		OldWeb:       j(legacyWebFilePath),
		OldUsrBin:    j(legacyUsrBinPath),
		NewConfigDir: j(newConfigDirPath),
		NewStateDir:  j(StateDir),
		NewBin:       j(newBinFilePath),
	}
}

func defaultMigrationPaths() migrationPaths { return migrationPathsAt(legacyRoot) }

// migrationOps is everything the migration does that tests must fake:
// systemctl, the primary directory rename (to inject EXDEV), chown (not
// possible to other ids without root) and a checkpoint before each step (to
// simulate a crash there).
type migrationOps interface {
	Systemctl(args ...string) (string, error)
	Rename(oldpath, newpath string) error
	Lchown(path string, uid, gid int) error
	Checkpoint(step string) error
}

type osMigrationOps struct{}

func (osMigrationOps) Systemctl(args ...string) (string, error) {
	out, err := osExec{}.Run("systemctl", args...)
	return string(out), err
}
func (osMigrationOps) Rename(o, n string) error            { return os.Rename(o, n) }
func (osMigrationOps) Lchown(p string, uid, gid int) error { return os.Lchown(p, uid, gid) }
func (osMigrationOps) Checkpoint(string) error             { return nil }

// migrationSteps, in order. Each is preceded by ops.Checkpoint(step).
var migrationSteps = []string{
	"stop-old-service",
	"mark-in-progress",
	"move-state",
	"move-config",
	"rewrite-config",
	"remove-old-install",
	"install",
	"compat-symlink",
	"write-marker",
	"clear-in-progress",
}

// dirPair is one legacy dir and where it moves.
type dirPair struct{ name, old, new string }

func (p migrationPaths) pairs() []dirPair {
	return []dirPair{
		{"state", p.OldStateDir, p.NewStateDir},
		{"config", p.OldConfigDir, p.NewConfigDir},
	}
}

// legacyPlan is a decision to migrate (or resume migrating) this host.
type legacyPlan struct {
	paths    migrationPaths
	resuming bool
}

// planLegacyMigration inspects the host without changing anything. It returns
// (nil, nil) when there is nothing to migrate (fresh host, trinetra-only host,
// or a finished migration), a plan when a serverwatch install is present (or a
// previous migration was interrupted), and an error when it must refuse.
func planLegacyMigration(p migrationPaths) (*legacyPlan, error) {
	legacy, resuming := false, false
	var oldFound, foreign []string
	for _, d := range p.pairs() {
		if lexists(d.old) {
			fi, err := os.Stat(d.old)
			if err != nil || !fi.IsDir() {
				return nil, fmt.Errorf("%s exists but is not a directory; refusing to migrate. Nothing was changed", d.old)
			}
			legacy = true
			oldFound = append(oldFound, d.old)
		}
		switch classifyNewDir(d.new) {
		case newDirOurs:
			resuming = true
			if lexists(d.old) && !hasMarker(d.old) && !isEmptyDir(d.old) {
				return nil, fmt.Errorf("both %s and %s exist, and %s was not left by an interrupted migration; refusing to merge or delete either. Nothing was changed. Inspect both and remove the one you do not need, then re-run `sudo trinetra install`", d.old, d.new, d.old)
			}
		case newDirForeign:
			foreign = append(foreign, d.new)
		}
	}
	if legacy && len(foreign) > 0 {
		return nil, fmt.Errorf("found both a serverwatch install (%s) and a trinetra install (%s); refusing to merge them. Nothing was changed.\n"+
			"If the trinetra paths hold nothing you need (for example a test install), stop it (sudo systemctl stop trinetra), move them aside (sudo mv %s %s.bak) and re-run `sudo trinetra install`.\n"+
			"Otherwise keep trinetra's data and archive or remove the serverwatch paths yourself",
			strings.Join(oldFound, ", "), strings.Join(foreign, ", "), foreign[0], foreign[0])
	}
	if !legacy && !resuming {
		return nil, nil
	}
	return &legacyPlan{paths: p, resuming: resuming}, nil
}

type newDirState int

const (
	newDirAbsent  newDirState = iota
	newDirEmpty               // an empty dir: treated as absent
	newDirOurs                // carries our in-progress marker
	newDirForeign             // anything else: a real trinetra install
)

func classifyNewDir(path string) newDirState {
	if !lexists(path) {
		return newDirAbsent
	}
	if hasMarker(path) {
		return newDirOurs
	}
	if isEmptyDir(path) {
		return newDirEmpty
	}
	return newDirForeign
}

func lexists(path string) bool { _, err := os.Lstat(path); return err == nil }

func hasMarker(dir string) bool { return lexists(filepath.Join(dir, migratingMarker)) }

func isEmptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

// migrationSummary reports what the migration did.
type migrationSummary struct {
	lines []string
	notes []string
}

func (s *migrationSummary) add(format string, a ...any) {
	s.lines = append(s.lines, fmt.Sprintf(format, a...))
}

func (s *migrationSummary) String() string {
	var b strings.Builder
	b.WriteString("migrated serverwatch -> trinetra:\n")
	for _, l := range s.lines {
		b.WriteString("  " + l + "\n")
	}
	for _, n := range s.notes {
		b.WriteString("note: " + n + "\n")
	}
	return b.String()
}

// migrationError is a stopped migration: the step, the cause, and a report of
// the current state with how to finish or roll back.
type migrationError struct {
	step  string
	err   error
	paths migrationPaths
}

func (e *migrationError) Unwrap() error { return e.err }

func (e *migrationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "migration from serverwatch stopped at step %q: %v\n", e.step, e.err)
	b.WriteString("No data was deleted that had not first been verified at its new location.\n")
	b.WriteString(describeMigrationState(e.paths))
	return strings.TrimRight(b.String(), "\n")
}

// describeMigrationState prints every relevant path's state and the manual
// ways forward (finish, or roll back) given that state.
func describeMigrationState(p migrationPaths) string {
	var b strings.Builder
	b.WriteString("Current state:\n")
	show := func(path, meaning string) {
		fi, err := os.Lstat(path)
		state := "absent"
		if err == nil {
			switch {
			case fi.Mode()&os.ModeSymlink != 0:
				l, _ := os.Readlink(path)
				state = "symlink -> " + l
			case fi.IsDir():
				state = "present"
				if hasMarker(path) {
					state += " (contains " + migratingMarker + ")"
				}
			default:
				state = "present"
			}
		}
		fmt.Fprintf(&b, "  %-45s %s", path, state)
		if meaning != "" && err == nil {
			b.WriteString("  -- " + meaning)
		}
		b.WriteString("\n")
	}
	for _, d := range p.pairs() {
		show(d.old, "")
		show(d.new+stagingSuffix, "partial copy; the source is intact, safe to delete")
		show(d.new, "")
	}
	show(p.OldUnit, "")
	show(p.OldBin, "")
	show(p.NewBin, "")

	b.WriteString("To finish: fix the cause above and re-run `sudo trinetra install`; it resumes where it stopped.\n")
	b.WriteString("To roll back by hand instead (as root):\n")
	for _, d := range p.pairs() {
		if lexists(d.new + stagingSuffix) {
			fmt.Fprintf(&b, "  rm -rf %s\n", d.new+stagingSuffix)
		}
		switch {
		case lexists(d.old) && lexists(d.new) && hasMarker(d.new):
			fmt.Fprintf(&b, "  # %s is a verified copy of %s, which was being removed\n", d.new, d.old)
			fmt.Fprintf(&b, "  rm -rf %s && mv %s %s\n", d.old, d.new, d.old)
		case !lexists(d.old) && lexists(d.new) && hasMarker(d.new):
			fmt.Fprintf(&b, "  mv %s %s\n", d.new, d.old)
		}
		fmt.Fprintf(&b, "  rm -f %s\n", filepath.Join(d.old, migratingMarker))
	}
	fmt.Fprintf(&b, "  # config values under %s/ and %s/ may point at the new dirs; change them back with `serverwatch config set`\n", newConfigDirPath, StateDir)
	if lexists(p.OldUnit) {
		fmt.Fprintf(&b, "  systemctl enable --now %s\n", legacyServiceName)
	} else {
		fmt.Fprintf(&b, "  # the %s unit was removed: reinstall it with the previous serverwatch release binary (`sudo ./serverwatch install`)\n", legacyServiceName)
	}
	b.WriteString("  systemctl disable --now trinetra 2>/dev/null; rm -f /etc/systemd/system/trinetra.service\n")
	return b.String()
}

// applyLegacyMigration carries out plan. install runs the normal install
// (binary, plugins, manifest, trinetra.service) at its place in the sequence;
// the compat symlink comes after it because the binary running `install` may
// itself live at /usr/local/bin/serverwatch.
func applyLegacyMigration(plan *legacyPlan, ops migrationOps, install func() error) (*migrationSummary, error) {
	p := plan.paths
	sum := &migrationSummary{}
	run := func(step string, fn func() error) error {
		if err := ops.Checkpoint(step); err != nil {
			return &migrationError{step: step, err: err, paths: p}
		}
		if err := fn(); err != nil {
			return &migrationError{step: step, err: err, paths: p}
		}
		return nil
	}
	moves := map[string]string{}
	steps := []struct {
		name string
		fn   func() error
	}{
		{"stop-old-service", func() error { return stopLegacyService(ops) }},
		{"mark-in-progress", func() error {
			for _, d := range p.pairs() {
				if lexists(d.old) && !hasMarker(d.old) {
					if err := writeFileSync(filepath.Join(d.old, migratingMarker), []byte("trinetra install is moving this directory to "+d.new+"\n"), 0o600); err != nil {
						return err
					}
				}
			}
			return nil
		}},
		{"move-state", func() error { return moveOne(p.pairs()[0], ops, moves) }},
		{"move-config", func() error { return moveOne(p.pairs()[1], ops, moves) }},
		{"rewrite-config", func() error {
			changed, err := rewriteLegacyConfigPaths(filepath.Join(p.NewConfigDir, "config.json"), ops)
			if err != nil {
				var pe *configParseError
				if errors.As(err, &pe) {
					sum.notes = append(sum.notes, fmt.Sprintf("config left as is (not valid JSON: %v); check web.tls_cert/web.tls_key by hand", err))
					return nil
				}
				return err
			}
			for _, c := range changed {
				sum.add("config %s", c)
			}
			return nil
		}},
		{"remove-old-install", func() error {
			if lexists(p.OldDropIn) {
				sum.notes = append(sum.notes, fmt.Sprintf("%s holds local overrides for the old unit; it was left in place -- copy what you need to /etc/systemd/system/trinetra.service.d/", p.OldDropIn))
			}
			for _, f := range []string{p.OldUnit, p.OldCtl, p.OldWeb} {
				if lexists(f) {
					if err := os.Remove(f); err != nil {
						return err
					}
					sum.add("removed %s", f)
				}
			}
			// /usr/bin/serverwatch only when it is still our symlink.
			if l, err := os.Readlink(p.OldUsrBin); err == nil && l == p.OldBin {
				if err := os.Remove(p.OldUsrBin); err != nil {
					return err
				}
				sum.add("removed %s", p.OldUsrBin)
			}
			return nil
		}},
		{"install", install},
		{"compat-symlink", func() error {
			if l, err := os.Readlink(p.OldBin); err == nil && l == p.NewBin {
				return nil
			}
			tmp := p.OldBin + ".tmp-compat"
			_ = os.Remove(tmp)
			if err := os.Symlink(p.NewBin, tmp); err != nil {
				return err
			}
			if err := os.Rename(tmp, p.OldBin); err != nil {
				_ = os.Remove(tmp)
				return err
			}
			sum.add("%s is now a symlink to %s (compat, removed in the next release)", p.OldBin, p.NewBin)
			return nil
		}},
		{"write-marker", func() error {
			m := filepath.Join(p.NewStateDir, migratedFromServerwatchMarker)
			if lexists(m) {
				return nil
			}
			if err := os.MkdirAll(p.NewStateDir, 0o755); err != nil {
				return err
			}
			return writeFileSync(m, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
		}},
		{"clear-in-progress", func() error {
			for _, d := range p.pairs() {
				if err := os.Remove(filepath.Join(d.new, migratingMarker)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		}},
	}
	for i, s := range steps {
		if s.name != migrationSteps[i] {
			panic("migration step table out of sync: " + s.name)
		}
		if err := run(s.name, s.fn); err != nil {
			return nil, err
		}
	}
	for _, d := range p.pairs() {
		if m, ok := moves[d.name]; ok {
			sum.lines = append([]string{m}, sum.lines...)
		}
	}
	return sum, nil
}

// stopLegacyService stops and disables serverwatch.service (absent unit is
// fine) and then insists it is really not running: moving the state dir out
// from under a live daemon would lose writes or recreate the old dir.
func stopLegacyService(ops migrationOps) error {
	_, _ = ops.Systemctl("stop", legacyServiceName)
	_, _ = ops.Systemctl("disable", legacyServiceName)
	out, _ := ops.Systemctl("is-active", legacyServiceName)
	switch strings.TrimSpace(out) {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return fmt.Errorf("%s.service is still running (systemctl is-active: %s) after `systemctl stop`; stop it and re-run", legacyServiceName, strings.TrimSpace(out))
	}
	return nil
}

func moveOne(d dirPair, ops migrationOps, moves map[string]string) error {
	msg, err := moveLegacyDir(d.old, d.new, ops)
	if err != nil {
		return err
	}
	if msg != "" {
		moves[d.name] = msg
	}
	return nil
}

// moveLegacyDir moves old to new, resuming whatever an interrupted earlier run
// left behind. It returns a summary line ("" when there was nothing to move).
func moveLegacyDir(old, newp string, ops migrationOps) (string, error) {
	staging := newp + stagingSuffix
	oldExists := lexists(old)
	state := classifyNewDir(newp)
	switch {
	case !oldExists:
		// Nothing (left) to move for this pair.
		if state == newDirOurs {
			return fmt.Sprintf("moved %s -> %s", old, newp), nil
		}
		return "", nil
	case state == newDirOurs:
		// A verified copy was already renamed into place; the source was
		// being removed when the previous run stopped.
		if !hasMarker(old) && !isEmptyDir(old) {
			return "", fmt.Errorf("both %s and %s exist and %s carries no migration marker; refusing to delete it", old, newp, old)
		}
		if err := removeVerifiedSource(old); err != nil {
			return "", fmt.Errorf("remove %s (already copied to %s): %w", old, newp, err)
		}
		return fmt.Sprintf("moved %s -> %s (copied, verified)", old, newp), nil
	case state == newDirForeign:
		return "", fmt.Errorf("%s already exists; refusing to merge %s into it", newp, old)
	}
	if state == newDirEmpty {
		if err := os.Remove(newp); err != nil {
			return "", err
		}
	}
	// A staging dir next to an intact source is a copy that did not finish.
	if lexists(staging) {
		if err := os.RemoveAll(staging); err != nil {
			return "", fmt.Errorf("remove leftover partial copy %s: %w", staging, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(newp), 0o755); err != nil {
		return "", err
	}
	err := ops.Rename(old, newp)
	if err == nil {
		return fmt.Sprintf("moved %s -> %s", old, newp), nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		if errors.Is(err, syscall.EBUSY) {
			return "", fmt.Errorf("%w (is %s a mount point? mount it at %s instead, then re-run)", err, old, newp)
		}
		return "", err
	}
	files, size, err := copyTreeVerified(old, staging, ops)
	if err != nil {
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			return "", fmt.Errorf("copy %s -> %s: %w (and removing the partial copy failed: %v)", old, staging, err, rmErr)
		}
		return "", fmt.Errorf("copy %s -> %s: %w (partial copy removed; %s is untouched)", old, staging, err, old)
	}
	if err := os.Rename(staging, newp); err != nil {
		return "", fmt.Errorf("rename verified copy %s -> %s: %w", staging, newp, err)
	}
	if err := removeVerifiedSource(old); err != nil {
		return "", fmt.Errorf("remove %s (already copied to %s): %w", old, newp, err)
	}
	return fmt.Sprintf("moved %s -> %s (copied across filesystems: %d files, %d bytes, verified)", old, newp, files, size), nil
}

// removeVerifiedSource deletes a legacy dir whose verified copy is in place.
// The in-progress marker goes last, so a crash part-way leaves a dir a re-run
// still recognises as ours to finish removing.
func removeVerifiedSource(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == migratingMarker {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, migratingMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(dir)
}

// treeEntry describes one entry for copy verification.
type treeEntry struct {
	mode     fs.FileMode
	size     int64
	uid, gid uint32
	link     string
	sum      [sha256.Size]byte
}

// copyTreeVerified copies src to dst (which must not exist), preserving
// modes (including setuid/setgid/sticky), ownership, symlinks and mtimes, then
// re-reads dst and checks every entry against src. Sockets and FIFOs hold no
// data and are skipped. It returns the number of non-directory entries and
// the total bytes of regular files copied.
func copyTreeVerified(src, dst string, ops migrationOps) (int, int64, error) {
	want := map[string]treeEntry{}
	var dirs []string
	var dirInfo = map[string]fs.FileInfo{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		e := entryFor(info)
		switch {
		case info.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, rel)
			dirInfo[rel] = info
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(l, target); err != nil {
				return err
			}
			if err := ops.Lchown(target, int(e.uid), int(e.gid)); err != nil {
				return err
			}
			e.link = l
		case info.Mode().IsRegular():
			sum, err := copyRegularFile(path, target, info, ops)
			if err != nil {
				return err
			}
			e.sum = sum
		case info.Mode()&(fs.ModeSocket|fs.ModeNamedPipe) != 0:
			return nil
		default:
			return fmt.Errorf("%s: unsupported file type %v", path, info.Mode().Type())
		}
		want[rel] = e
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	// Directory ownership, modes and times last (deepest first), so a
	// read-only dir does not block creating its children and child writes do
	// not bump the parent's mtime afterwards.
	for i := len(dirs) - 1; i >= 0; i-- {
		rel := dirs[i]
		info := dirInfo[rel]
		e := want[rel]
		target := filepath.Join(dst, rel)
		if err := ops.Lchown(target, int(e.uid), int(e.gid)); err != nil {
			return 0, 0, err
		}
		if err := os.Chmod(target, chmodBits(info.Mode())); err != nil {
			return 0, 0, err
		}
		if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
			return 0, 0, err
		}
	}
	return verifyTree(dst, want)
}

func entryFor(info fs.FileInfo) treeEntry {
	e := treeEntry{mode: info.Mode()}
	if info.Mode().IsRegular() {
		e.size = info.Size()
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.uid, e.gid = st.Uid, st.Gid
	}
	return e
}

func chmodBits(m fs.FileMode) fs.FileMode {
	return m & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func copyRegularFile(src, dst string, info fs.FileInfo, ops migrationOps) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	in, err := os.Open(src)
	if err != nil {
		return sum, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return sum, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil && n != info.Size() {
		err = fmt.Errorf("%s: copied %d bytes, expected %d", src, n, info.Size())
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return sum, err
	}
	e := entryFor(info)
	// chown before chmod: chown clears setuid/setgid bits.
	if err := ops.Lchown(dst, int(e.uid), int(e.gid)); err != nil {
		return sum, err
	}
	if err := os.Chmod(dst, chmodBits(info.Mode())); err != nil {
		return sum, err
	}
	if err := os.Chtimes(dst, info.ModTime(), info.ModTime()); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// verifyTree walks dst and requires it to match want exactly.
func verifyTree(dst string, want map[string]treeEntry) (int, int64, error) {
	seen := 0
	files := 0
	var size int64
	err := filepath.WalkDir(dst, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		w, ok := want[rel]
		if !ok {
			return fmt.Errorf("verify: unexpected %s in copy", path)
		}
		seen++
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		got := entryFor(info)
		if got.mode != w.mode || got.size != w.size || got.uid != w.uid || got.gid != w.gid {
			return fmt.Errorf("verify %s: got mode=%v size=%d owner=%d:%d, want mode=%v size=%d owner=%d:%d",
				path, got.mode, got.size, got.uid, got.gid, w.mode, w.size, w.uid, w.gid)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if l != w.link {
				return fmt.Errorf("verify %s: symlink -> %q, want %q", path, l, w.link)
			}
			files++
		case info.Mode().IsRegular():
			s, err := sha256File(path)
			if err != nil {
				return err
			}
			if s != fmt.Sprintf("%x", w.sum) {
				return fmt.Errorf("verify %s: content differs from source", path)
			}
			files++
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if seen != len(want) {
		return 0, 0, fmt.Errorf("verify: copy has %d entries, source had %d", seen, len(want))
	}
	return files, size, nil
}

// Path-valued config keys that may point inside the legacy dirs, as
// section/key pairs of the config JSON. Keep this list explicit.
var legacyPathConfigKeys = [][2]string{
	{"web", "tls_cert"},
	{"web", "tls_key"},
}

var legacyPathPrefixes = [][2]string{
	{legacyConfigDirPath, newConfigDirPath},
	{legacyStateDirPath, StateDir},
}

// rewriteLegacyPath maps a path inside a legacy dir to the new dir.
func rewriteLegacyPath(v string) string {
	for _, pr := range legacyPathPrefixes {
		if v == pr[0] || strings.HasPrefix(v, pr[0]+"/") {
			return pr[1] + v[len(pr[0]):]
		}
	}
	return v
}

// rewriteLegacyConfigPaths rewrites the path-valued keys of the config at
// path that point inside the legacy dirs. It edits the JSON generically (not
// through config.Config) so keys this build does not know are kept, and
// writes atomically with the original mode and owner. The file is left
// untouched when nothing needs rewriting. It returns "key: old -> new" lines.
func rewriteLegacyConfigPaths(path string, ops migrationOps) ([]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, &configParseError{path: path, err: err}
	}
	var changed []string
	sections := map[string]map[string]json.RawMessage{}
	for _, k := range legacyPathConfigKeys {
		raw, ok := top[k[0]]
		if !ok {
			continue
		}
		sec, ok := sections[k[0]]
		if !ok {
			if err := json.Unmarshal(raw, &sec); err != nil || sec == nil {
				continue // not an object: nothing of ours to rewrite
			}
			sections[k[0]] = sec
		}
		var v string
		if err := json.Unmarshal(sec[k[1]], &v); err != nil {
			continue
		}
		nv := rewriteLegacyPath(v)
		if nv == v {
			continue
		}
		enc, err := json.Marshal(nv)
		if err != nil {
			return nil, err
		}
		sec[k[1]] = enc
		changed = append(changed, fmt.Sprintf("%s.%s: %s -> %s", k[0], k[1], v, nv))
	}
	if len(changed) == 0 {
		return nil, nil
	}
	for name, sec := range sections {
		enc, err := json.Marshal(sec)
		if err != nil {
			return nil, err
		}
		top[name] = enc
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	tmp := path + ".tmp-migrate"
	if err := writeFileSync(tmp, out, 0o600); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	e := entryFor(fi)
	if err := ops.Lchown(tmp, int(e.uid), int(e.gid)); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Chmod(tmp, chmodBits(fi.Mode())); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	sort.Strings(changed)
	return changed, nil
}

// configParseError: the config is not valid JSON. The migration leaves such a
// file exactly as it is and notes it, rather than failing.
type configParseError struct {
	path string
	err  error
}

func (e *configParseError) Error() string { return fmt.Sprintf("parse %s: %v", e.path, e.err) }
func (e *configParseError) Unwrap() error { return e.err }

// writeFileSync writes and fsyncs a file (mode applies on create).
func writeFileSync(path string, data []byte, perm fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// legacyInstallGuard stops the daemon from starting empty on a host that
// still has only the serverwatch paths: the operator must migrate first. A
// fresh host (neither old nor new paths) is the normal first run and passes.
func legacyInstallGuard(p migrationPaths) error {
	if lexists(cfgPath) || lexists(stateDir) {
		return nil
	}
	var found []string
	for _, d := range []string{p.OldConfigDir, p.OldStateDir} {
		if lexists(d) {
			found = append(found, d)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("found a serverwatch install at %s; run `sudo trinetra install` to migrate", strings.Join(found, " and "))
}
