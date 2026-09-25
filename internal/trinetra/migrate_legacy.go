package trinetra

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
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
//   - planLegacyMigration only looks; it never changes anything. It refuses
//     (never merges) when a legacy dir and its trinetra counterpart both hold
//     data it cannot prove it produced, when a legacy dir is a mount point, or
//     when the new path aliases the old one.
//   - Before any data moves, an in-progress marker (migratingMarker) holding a
//     random run token is written into each legacy dir.
//   - A directory moves by rename(2) (atomic: the marker moves with it and the
//     old path is gone, so nothing is ever deleted on this path). On EXDEV it
//     is copied into a staging sibling of the destination (without the
//     in-progress marker) and the copy is verified entry by entry (type, mode,
//     owner, size, symlink target, SHA-256). Only then is a DISTINCT
//     copy-verified marker carrying the same token written into it, and the
//     staging dir renamed into place.
//   - A source is deleted only when the new dir carries a copy-verified marker
//     whose token matches the source's in-progress marker, the two are not
//     the same directory by any route (same file, symlink, bind mount,
//     nested alias), and, when resuming, every entry of the source is
//     re-verified as present and identical in the new dir. The source's
//     marker is deleted last, so a partly deleted source is still recognised.
//   - Every step is safe to re-run after a crash at any point; the markers in
//     the new dirs are removed only as the very last step.
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
	// legacyModulePath is the Go module the serverwatch releases were built
	// from; its build info identifies a serverwatch binary.
	legacyModulePath = "serverwatch"

	newConfigDirPath = "/etc/trinetra"
	newBinFilePath   = "/usr/local/bin/trinetra"

	// migratingMarker is written into each legacy dir before it moves. Its
	// first line is the run token. After a rename it sits in the new dir.
	migratingMarker = ".migrating-from-serverwatch"
	// copyVerifiedMarker is written into a staging copy only after the copy
	// verified; its first line is the token of the source it copies.
	copyVerifiedMarker = ".serverwatch-copy-verified"
	// migratedFromServerwatchMarker (in the new state dir) records that, and
	// when, this host was migrated.
	migratedFromServerwatchMarker = "migrated-from-serverwatch"
	// stagingSuffix names the sibling of a new dir that an EXDEV copy is
	// written into before being renamed into place.
	stagingSuffix = ".migrating"
)

// legacyRoot prefixes every migration path; tests point it at a temp dir.
var legacyRoot = ""

// Seams for tests: mount-point detection and "is this our serverwatch binary".
var (
	isMountPointFn      = isMountPoint
	isMountTargetFn     = isMountTarget
	isOurLegacyBinaryFn = isOurLegacyBinary
	mountinfoPath       = "/proc/self/mountinfo"
	fstabPath           = "/etc/fstab"
	mountUnitDirs       = []string{"/etc/systemd/system", "/run/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}
	// progressOut receives progress lines for long walks (nil: silent);
	// cmdInstall points it at stdout.
	progressOut io.Writer
)

func progressf(format string, a ...any) {
	if progressOut != nil {
		fmt.Fprintf(progressOut, format+"\n", a...)
	}
}

const errMigrationRefusedF = "refusing to migrate. Nothing was changed"

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
// systemctl, the primary directory rename (to inject EXDEV/EBUSY), chown (not
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
// remove-old-install comes after install so a failed install leaves the old
// unit and plugins in place.
var migrationSteps = []string{
	"stop-old-service",
	"mark-in-progress",
	"move-state",
	"move-config",
	"rewrite-config",
	"install",
	"remove-old-install",
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
	// force proceeds when systemctl cannot confirm the old service stopped.
	force bool
	notes []string
}

// planOptions are the operator's explicit choices for planLegacyMigration.
type planOptions struct {
	// stateAtNewPath (install --state-already-at-new-path): the operator
	// moved the serverwatch state volume to the trinetra state path, so an
	// existing trinetra state dir next to the legacy config is adopted. It
	// is never inferred.
	stateAtNewPath bool
}

// planLegacyMigration inspects the host without changing anything. It returns
// (nil, nil) when there is nothing to migrate (fresh host, trinetra-only host,
// or a finished migration), a plan when a serverwatch install is present (or a
// previous migration was interrupted), and an error when it must refuse.
func planLegacyMigration(p migrationPaths, opts planOptions) (*legacyPlan, error) {
	plan := &legacyPlan{paths: p}
	type pairInfo struct {
		d            dirPair
		exists, data bool
		ns           newDirStatus
	}
	var infos []pairInfo
	legacy := false
	var oldFound []string
	for _, d := range p.pairs() {
		data, err := legacyDirPresent(d.old)
		if err != nil {
			return nil, err
		}
		infos = append(infos, pairInfo{d: d, exists: lexists(d.old), data: data, ns: inspectNewDir(d.new)})
		if data {
			legacy = true
			oldFound = append(oldFound, d.old)
		}
	}
	// A legacy dir that exists but is empty is suspicious, never "absent":
	// most likely its volume is not mounted this boot, and migrating the rest
	// would strand the real data there.
	for _, in := range infos {
		if !in.exists || in.data {
			continue
		}
		if isMountTargetFn(in.d.old) || isMountPointFn(in.d.old) {
			return nil, fmt.Errorf("%s is empty but is a mount point or a mount target (/etc/fstab or a systemd .mount unit): is its volume mounted? "+
				"Check /etc/fstab and `systemctl list-units --type=mount`, mount it, and re-run `sudo trinetra install`. "+
				"If you moved the volume to %s yourself, remove the old mount entry first. %s", in.d.old, in.d.new, errMigrationRefusedF)
		}
		if in.ns.ours() {
			continue // the tail of our own interrupted source removal
		}
		if legacy {
			if in.d.name == "state" && opts.stateAtNewPath {
				continue
			}
			hint := ""
			if in.d.name == "state" {
				hint = fmt.Sprintf("If you moved the serverwatch state volume to %s yourself, re-run with `sudo trinetra install --state-already-at-new-path`. ", in.d.new)
			}
			return nil, fmt.Errorf("%s is empty while %s still holds data: is its volume mounted? Check /etc/fstab and `systemctl list-units --type=mount`. %s%s",
				in.d.old, strings.Join(oldFound, ", "), hint, errMigrationRefusedF)
		}
	}
	var conflicts []string
	for _, in := range infos {
		d, ns := in.d, in.ns
		if !in.data {
			if ns.ours() {
				plan.resuming = true
			}
			if d.name == "state" && opts.stateAtNewPath && legacy {
				if ns.kind != newDirForeign {
					return nil, fmt.Errorf("--state-already-at-new-path was given but %s holds no existing trinetra state to adopt; %s", d.new, errMigrationRefusedF)
				}
				plan.notes = append(plan.notes, fmt.Sprintf("ADOPTED the existing state at %s as trinetra's state (--state-already-at-new-path); nothing was moved from %s", d.new, d.old))
				continue
			}
			if ns.kind == newDirForeign && legacy {
				conflicts = append(conflicts, fmt.Sprintf("%s already holds data", d.new))
			}
			continue
		}
		if d.name == "state" && opts.stateAtNewPath {
			return nil, fmt.Errorf("--state-already-at-new-path was given but %s still holds data; %s", d.old, errMigrationRefusedF)
		}
		if isMountPointFn(d.old) {
			advice := fmt.Sprintf("mount it at %s instead (update /etc/fstab), leave %s empty or remove it, then re-run `sudo trinetra install --state-already-at-new-path`", d.new, d.old)
			if d.name != "state" {
				advice = fmt.Sprintf("copy its contents into a plain directory at %s on the root filesystem, then re-run `sudo trinetra install`", d.old)
			}
			return nil, fmt.Errorf("%s is a mount point, so it cannot be moved by renaming; %s.\n"+
				"Stop serverwatch (sudo systemctl stop serverwatch), unmount the volume from %s first, then %s.\n"+
				"Do not mount the volume at %s while it is still mounted at %s",
				d.old, errMigrationRefusedF, d.old, advice, d.new, d.old)
		}
		switch ns.kind {
		case newDirAbsent, newDirEmpty:
		case newDirSymlink:
			return nil, fmt.Errorf("%s is a symlink (-> %s) while %s still holds data; %s.\n"+
				"Remove the symlink (the migration creates %s itself) and re-run `sudo trinetra install`", d.new, ns.link, d.old, errMigrationRefusedF, d.new)
		case newDirCopied:
			oldTok, _ := readMarkerToken(filepath.Join(d.old, migratingMarker))
			if oldTok == "" || oldTok != ns.token {
				return nil, fmt.Errorf("%s holds a copy-verified marker that does not match %s (token %q vs %q); %s.\n"+
					"Inspect both, keep the one with your data, and re-run `sudo trinetra install`", d.new, d.old, ns.token, oldTok, errMigrationRefusedF)
			}
			if err := checkNotAliased(d.old, d.new); err != nil {
				return nil, fmt.Errorf("%v; %s", err, errMigrationRefusedF)
			}
			plan.resuming = true
		case newDirRenamed:
			conflicts = append(conflicts, fmt.Sprintf("%s carries only an in-progress marker, not a verified copy, while %s still holds data (a manual copy, bind mount or symlink?)", d.new, d.old))
		default:
			conflicts = append(conflicts, fmt.Sprintf("%s already holds data", d.new))
		}
	}
	if legacy && len(conflicts) > 0 {
		return nil, fmt.Errorf("found both a serverwatch install (%s) and trinetra data: %s; refusing to merge them. Nothing was changed.\n"+
			"If the trinetra paths hold nothing you need (for example a test install), stop it (sudo systemctl stop trinetra), move them aside (e.g. sudo mv %s %s.bak) and re-run `sudo trinetra install`.\n"+
			"If you moved the serverwatch state volume to %s yourself, re-run with `sudo trinetra install --state-already-at-new-path`.\n"+
			"Otherwise keep trinetra's data and archive or remove the serverwatch paths yourself",
			strings.Join(oldFound, ", "), strings.Join(conflicts, "; "), p.NewStateDir, p.NewStateDir, p.NewStateDir)
	}
	if !legacy && !plan.resuming {
		return nil, nil
	}
	return plan, nil
}

// legacyDirPresent reports whether a legacy dir holds anything. An empty dir
// is not data, but the planner treats it as suspicious (see above).
func legacyDirPresent(path string) (bool, error) {
	if !lexists(path) {
		return false, nil
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return false, fmt.Errorf("%s exists but is not a directory; %s", path, errMigrationRefusedF)
	}
	return !isEmptyDir(path), nil
}

type newDirKind int

const (
	newDirAbsent  newDirKind = iota
	newDirEmpty              // an empty dir: treated as absent
	newDirSymlink            // the new path is a symlink
	newDirRenamed            // carries the in-progress marker only (a completed rename)
	newDirCopied             // carries the copy-verified marker only
	newDirForeign            // anything else: a real trinetra install
)

type newDirStatus struct {
	kind  newDirKind
	token string
	link  string
}

func (s newDirStatus) ours() bool { return s.kind == newDirRenamed || s.kind == newDirCopied }

func inspectNewDir(path string) newDirStatus {
	fi, err := os.Lstat(path)
	if err != nil {
		return newDirStatus{kind: newDirAbsent}
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		l, _ := os.Readlink(path)
		return newDirStatus{kind: newDirSymlink, link: l}
	}
	mig := lexists(filepath.Join(path, migratingMarker))
	ver := lexists(filepath.Join(path, copyVerifiedMarker))
	switch {
	case ver && !mig:
		tok, _ := readMarkerToken(filepath.Join(path, copyVerifiedMarker))
		return newDirStatus{kind: newDirCopied, token: tok}
	case mig && !ver:
		tok, _ := readMarkerToken(filepath.Join(path, migratingMarker))
		return newDirStatus{kind: newDirRenamed, token: tok}
	case !mig && !ver && isEmptyDir(path):
		return newDirStatus{kind: newDirEmpty}
	}
	return newDirStatus{kind: newDirForeign}
}

func lexists(path string) bool { _, err := os.Lstat(path); return err == nil }

func isEmptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

func newRunToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// readMarkerToken returns the first line of a marker file.
func readMarkerToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line), nil
}

// isMountPoint reports whether dir (not a symlink) is a mount point: its
// device differs from its parent's, or the mount table lists it (a bind
// mount from the same filesystem keeps the device number).
func isMountPoint(dir string) bool {
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	pfi, err := os.Stat(filepath.Dir(dir))
	if err != nil {
		return false
	}
	if d1, ok1 := devIno(fi); ok1 {
		if d2, ok2 := devIno(pfi); ok2 && d1[0] != d2[0] {
			return true
		}
	}
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescapeMountinfo(fields[4]) == abs {
			return true
		}
	}
	return false
}

// isMountTarget reports whether dir is configured to have something mounted
// on it: a mount point in /etc/fstab (second field) or a systemd
// <escaped-path>.mount / .automount unit.
func isMountTarget(dir string) bool {
	clean := filepath.Clean(dir)
	if b, err := os.ReadFile(fstabPath); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := strings.Fields(line)
			if len(f) > 1 && filepath.Clean(unescapeMountinfo(f[1])) == clean {
				return true
			}
		}
	}
	unit := systemdEscapePath(clean)
	for _, d := range mountUnitDirs {
		for _, ext := range []string{".mount", ".automount"} {
			if lexists(filepath.Join(d, unit+ext)) {
				return true
			}
		}
	}
	return false
}

// systemdEscapePath is `systemd-escape --path`: strip slashes at both ends,
// "/" -> "-", and \xNN for bytes outside [A-Za-z0-9:_.] (and a leading ".").
func systemdEscapePath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && (i == 0 || p[i-1] == '/'):
			fmt.Fprintf(&b, `\x%02x`, c)
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_', c == '.':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// unescapeMountinfo decodes the \NNN octal escapes /proc/self/mountinfo uses.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func devIno(fi fs.FileInfo) ([2]uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]uint64{}, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, true
}

// checkNotAliased returns an error unless oldDir and newDir are genuinely two
// different directory trees: newDir must not be a symlink, must not be the
// same directory as oldDir (by path resolution or device+inode, which also
// catches bind mounts), must not resolve inside oldDir or contain it, and no
// directory inside one may be the same directory as one inside the other.
func checkNotAliased(oldDir, newDir string) error {
	nfi, err := os.Lstat(newDir)
	if err != nil {
		return err
	}
	if nfi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; cannot prove it is separate from %s", newDir, oldDir)
	}
	ofs, err := os.Stat(oldDir)
	if err != nil {
		return err
	}
	nfs, err := os.Stat(newDir)
	if err != nil {
		return err
	}
	if os.SameFile(ofs, nfs) {
		return fmt.Errorf("%s and %s are the same directory (bind mount or link)", newDir, oldDir)
	}
	ro, err1 := filepath.EvalSymlinks(oldDir)
	rn, err2 := filepath.EvalSymlinks(newDir)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("cannot resolve %s / %s: %v %v", oldDir, newDir, err1, err2)
	}
	if ro == rn || strings.HasPrefix(rn, ro+"/") || strings.HasPrefix(ro, rn+"/") {
		return fmt.Errorf("%s (%s) and %s (%s) overlap", newDir, rn, oldDir, ro)
	}
	oldDirs := map[[2]uint64]string{}
	if err := walkDirs(oldDir, func(path string, id [2]uint64) error {
		oldDirs[id] = path
		return nil
	}); err != nil {
		return err
	}
	return walkDirs(newDir, func(path string, id [2]uint64) error {
		if o, ok := oldDirs[id]; ok {
			return fmt.Errorf("%s is the same directory as %s (bind mount?)", path, o)
		}
		return nil
	})
}

// walkDirs calls fn with the device+inode of every directory under root
// (symlinks are not followed).
func walkDirs(root string, fn func(path string, id [2]uint64) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		id, ok := devIno(info)
		if !ok {
			return fmt.Errorf("no device/inode for %s", path)
		}
		return fn(path, id)
	})
}

// verifySubset requires every entry of oldDir (except its migration marker)
// to exist in newDir with the same type, mode, owner, size, link target and
// content, so deleting oldDir loses nothing.
func verifySubset(oldDir, newDir string) error {
	return filepath.WalkDir(oldDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(oldDir, path)
		if err != nil {
			return err
		}
		if rel == migratingMarker {
			return nil
		}
		oi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if oi.Mode()&(fs.ModeSocket|fs.ModeNamedPipe) != 0 {
			return nil
		}
		np := filepath.Join(newDir, rel)
		ni, err := os.Lstat(np)
		if err != nil {
			return fmt.Errorf("%s is not in %s: %v", path, newDir, err)
		}
		o, n := entryFor(oi), entryFor(ni)
		if o.mode != n.mode || o.size != n.size || o.uid != n.uid || o.gid != n.gid {
			return fmt.Errorf("%s differs from %s", np, path)
		}
		switch {
		case oi.Mode()&fs.ModeSymlink != 0:
			l1, err1 := os.Readlink(path)
			l2, err2 := os.Readlink(np)
			if err1 != nil || err2 != nil || l1 != l2 {
				return fmt.Errorf("symlink %s differs from %s", np, path)
			}
		case oi.Mode().IsRegular():
			s1, err1 := sha256File(path)
			s2, err2 := sha256File(np)
			if err1 != nil || err2 != nil || s1 != s2 {
				return fmt.Errorf("content of %s differs from %s", np, path)
			}
		}
		return nil
	})
}

// isOurLegacyBinary reports whether path is a Go binary built from the
// serverwatch (or trinetra) module, judged by its embedded build info.
func isOurLegacyBinary(path string) bool {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return false
	}
	return bi.Main.Path == legacyModulePath || bi.Main.Path == "github.com/InfoDiveLabs/trinetra" ||
		strings.HasPrefix(bi.Path, legacyModulePath+"/")
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
				if lexists(filepath.Join(path, migratingMarker)) {
					state += " (in-progress marker " + migratingMarker + ")"
				}
				if lexists(filepath.Join(path, copyVerifiedMarker)) {
					state += " (verified copy, marker " + copyVerifiedMarker + ")"
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
		if stagingSafeToDelete(d) {
			show(d.new+stagingSuffix, "unfinished copy; the source is intact, safe to delete")
		} else {
			show(d.new+stagingSuffix, "unfinished copy; do NOT delete it until the source is confirmed intact")
		}
		show(d.new, "")
	}
	show(p.OldUnit, "")
	show(p.OldBin, "")
	show(p.NewBin, "")

	b.WriteString("To finish: fix the cause above and re-run `sudo trinetra install`; it resumes where it stopped.\n")
	b.WriteString("To roll back by hand instead (as root):\n")
	for _, d := range p.pairs() {
		if stagingSafeToDelete(d) {
			fmt.Fprintf(&b, "  rm -rf %s\n", d.new+stagingSuffix)
		} else if lexists(d.new + stagingSuffix) {
			fmt.Fprintf(&b, "  # do NOT delete %s: %s is missing or shares its files, so it may hold the only copy\n", d.new+stagingSuffix, d.old)
		}
		ns := inspectNewDir(d.new)
		switch {
		case lexists(d.old) && ns.kind == newDirCopied && copyProvablyComplete(d.old, d.new, ns.token):
			fmt.Fprintf(&b, "  # %s is a verified, complete copy of %s, which was being removed; keep the copy:\n", d.new, d.old)
			fmt.Fprintf(&b, "  rm -rf %s && mv %s %s\n", d.old, d.new, d.old)
		case lexists(d.old) && lexists(d.new):
			fmt.Fprintf(&b, "  # do NOT delete either %s or %s: compare them (e.g. diff -r %s %s) and keep the one with the newest data\n", d.old, d.new, d.old, d.new)
		case !lexists(d.old) && ns.ours():
			fmt.Fprintf(&b, "  mv %s %s\n", d.new, d.old)
		}
		fmt.Fprintf(&b, "  rm -f %s %s\n", filepath.Join(d.old, migratingMarker), filepath.Join(d.old, copyVerifiedMarker))
	}
	fmt.Fprintf(&b, "  # edit %s/config.json directly (not with `serverwatch config set`, which now edits the trinetra config): web.tls_cert/web.tls_key may point under %s or %s\n",
		legacyConfigDirPath, newConfigDirPath, StateDir)
	if lexists(p.OldUnit) {
		fmt.Fprintf(&b, "  systemctl enable --now %s\n", legacyServiceName)
	} else {
		fmt.Fprintf(&b, "  # the %s unit was removed: reinstall it with the previous serverwatch release binary (`sudo ./serverwatch install`)\n", legacyServiceName)
	}
	b.WriteString("  systemctl disable --now trinetra 2>/dev/null; rm -f /etc/systemd/system/trinetra.service\n")
	return b.String()
}

// copyProvablyComplete: newDir is a verified copy made from oldDir's run
// (tokens match), is a separate tree, and holds everything oldDir holds, so
// deleting oldDir loses nothing.
// stagingSafeToDelete reports whether d's staging copy may be deleted by hand:
// only while the source still exists as a separate directory, since otherwise
// the staging copy may be the only one left.
func stagingSafeToDelete(d dirPair) bool {
	st := d.new + stagingSuffix
	return lexists(st) && lexists(d.old) && checkNotAliased(d.old, st) == nil
}

func copyProvablyComplete(oldDir, newDir, newTok string) bool {
	oldTok, _ := readMarkerToken(filepath.Join(oldDir, migratingMarker))
	return oldTok != "" && oldTok == newTok &&
		checkNotAliased(oldDir, newDir) == nil && verifySubset(oldDir, newDir) == nil
}

// applyLegacyMigration carries out plan. install runs the normal install
// (binary, plugins, manifest, trinetra.service) at its place in the sequence;
// the old unit/plugins are removed only after it succeeded, and the compat
// symlink comes after it because the binary running `install` may itself
// live at /usr/local/bin/serverwatch.
func applyLegacyMigration(plan *legacyPlan, ops migrationOps, install func() error) (*migrationSummary, error) {
	p := plan.paths
	sum := &migrationSummary{notes: append([]string(nil), plan.notes...)}
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
		{"stop-old-service", func() error {
			note, err := stopLegacyService(ops, plan.force)
			if note != "" {
				sum.notes = append(sum.notes, note)
			}
			return err
		}},
		{"mark-in-progress", func() error {
			for _, d := range p.pairs() {
				present, err := legacyDirPresent(d.old)
				if err != nil {
					return err
				}
				if !present || lexists(filepath.Join(d.old, migratingMarker)) {
					continue
				}
				tok, err := newRunToken()
				if err != nil {
					return err
				}
				body := tok + "\ntrinetra install is moving this directory to " + d.new + "\n"
				if err := writeFileSync(filepath.Join(d.old, migratingMarker), []byte(body), 0o600); err != nil {
					return err
				}
			}
			return nil
		}},
		{"move-state", func() error { return moveOne(p.pairs()[0], ops, moves) }},
		{"move-config", func() error { return moveOne(p.pairs()[1], ops, moves) }},
		{"rewrite-config", func() error {
			if links := symlinksIntoLegacyDirs(p); len(links) > 0 {
				sum.notes = append(sum.notes, "these symlinks still point into the old serverwatch paths (left as they are; fix them by hand):\n    "+strings.Join(links, "\n    "))
			}
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
		{"install", install},
		{"remove-old-install", func() error {
			if lexists(p.OldDropIn) {
				sum.notes = append(sum.notes, fmt.Sprintf("%s holds local overrides for the old unit; it was left in place -- copy what you need to /etc/systemd/system/trinetra.service.d/", p.OldDropIn))
			}
			removedUnit := false
			for _, f := range []string{p.OldUnit, p.OldCtl, p.OldWeb} {
				if lexists(f) {
					if err := os.Remove(f); err != nil {
						return err
					}
					sum.add("removed %s", f)
					removedUnit = removedUnit || f == p.OldUnit
				}
			}
			if removedUnit {
				_, _ = ops.Systemctl("daemon-reload")
			}
			return nil
		}},
		{"compat-symlink", func() error { return installCompatLinks(p, sum) }},
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
				for _, m := range []string{migratingMarker, copyVerifiedMarker} {
					if err := os.Remove(filepath.Join(d.new, m)); err != nil && !os.IsNotExist(err) {
						return err
					}
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

// installCompatLinks points /usr/local/bin/serverwatch at trinetra, but only
// replaces what is there when it is our own serverwatch binary (or already
// our link); anything else is kept and noted. /usr/bin/serverwatch stays (or
// becomes) a symlink to /usr/local/bin/serverwatch so `sudo serverwatch`
// keeps working on hosts whose secure_path omits /usr/local/bin.
func installCompatLinks(p migrationPaths, sum *migrationSummary) error {
	ours := false
	fi, err := os.Lstat(p.OldBin)
	switch {
	case err != nil:
		ours = true // absent: create it
	case fi.Mode()&os.ModeSymlink != 0:
		if l, _ := os.Readlink(p.OldBin); l == p.NewBin {
			ours = true
		} else {
			sum.notes = append(sum.notes, fmt.Sprintf("%s is a symlink to %s, not ours; left as is (the serverwatch compat name is not installed)", p.OldBin, l))
		}
	case fi.Mode().IsRegular() && isOurLegacyBinaryFn(p.OldBin):
		ours = true
	default:
		sum.notes = append(sum.notes, fmt.Sprintf("%s is not a serverwatch binary we recognise; left as is (the serverwatch compat name is not installed)", p.OldBin))
	}
	if !ours {
		return nil
	}
	if l, err := os.Readlink(p.OldBin); err != nil || l != p.NewBin {
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
	}
	switch l, err := os.Readlink(p.OldUsrBin); {
	case err == nil && l == p.OldBin:
		// already our link; it now resolves to trinetra through the compat link
	case !lexists(p.OldUsrBin):
		if err := os.MkdirAll(filepath.Dir(p.OldUsrBin), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(p.OldBin, p.OldUsrBin); err != nil {
			return err
		}
	}
	return nil
}

// symlinksIntoLegacyDirs lists absolute symlinks inside the new dirs whose
// targets are inside the old serverwatch paths (they would now dangle).
func symlinksIntoLegacyDirs(p migrationPaths) []string {
	prefixes := []string{legacyConfigDirPath, legacyStateDirPath, p.OldConfigDir, p.OldStateDir}
	var out []string
	for _, d := range p.pairs() {
		_ = filepath.WalkDir(d.new, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			l, err := os.Readlink(path)
			if err != nil || !filepath.IsAbs(l) {
				return nil
			}
			for _, pre := range prefixes {
				if l == pre || strings.HasPrefix(l, pre+"/") {
					out = append(out, path+" -> "+l)
					break
				}
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// stopLegacyService stops and disables serverwatch.service (absent unit is
// fine) and then insists systemd confirms it is not running: moving the state
// dir out from under a live daemon would lose writes or recreate the old dir.
// When systemctl cannot give an answer, only force proceeds.
func stopLegacyService(ops migrationOps, force bool) (string, error) {
	_, _ = ops.Systemctl("stop", legacyServiceName)
	_, _ = ops.Systemctl("disable", legacyServiceName)
	out, err := ops.Systemctl("is-active", legacyServiceName)
	state := strings.TrimSpace(out)
	switch state {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return "", fmt.Errorf("%s.service is still running (systemctl is-active: %s) after `systemctl stop`; stop it and re-run", legacyServiceName, state)
	case "inactive", "failed", "unknown":
		return "", nil
	}
	if force {
		return fmt.Sprintf("could not confirm %s.service is stopped (systemctl is-active: %q, %v); continued because of --force", legacyServiceName, state, err), nil
	}
	return "", fmt.Errorf("could not confirm %s.service is stopped (systemctl is-active printed %q, error %v). "+
		"Make sure no serverwatch daemon is running, then re-run with `sudo trinetra install --force`", legacyServiceName, state, err)
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
	present, err := legacyDirPresent(old)
	if err != nil {
		return "", err
	}
	ns := inspectNewDir(newp)
	if !present {
		// Nothing (left) to move. An empty old dir (a verified source whose
		// removal was interrupted after its marker went, or an unmounted
		// mount point) is removed; rmdir never removes anything with data.
		if lexists(old) && !isMountPointFn(old) && !isMountTargetFn(old) {
			if err := os.Remove(old); err != nil {
				return "", err
			}
		}
		if ns.ours() {
			return fmt.Sprintf("moved %s -> %s", old, newp), nil
		}
		return "", nil
	}
	oldTok, _ := readMarkerToken(filepath.Join(old, migratingMarker))
	if oldTok == "" {
		return "", fmt.Errorf("%s has no migration marker; refusing to move it", old)
	}
	switch ns.kind {
	case newDirCopied:
		// A verified copy was renamed into place; the source was being
		// removed when the previous run stopped. Prove it again first.
		if ns.token != oldTok {
			return "", fmt.Errorf("%s is a verified copy of a different source (token %q, %s has %q); refusing to delete %s", newp, ns.token, old, oldTok, old)
		}
		if err := checkNotAliased(old, newp); err != nil {
			return "", fmt.Errorf("%w; refusing to delete %s", err, old)
		}
		progressf("verifying %s against %s before removing it...", old, newp)
		if err := verifySubset(old, newp); err != nil {
			return "", fmt.Errorf("%w; refusing to delete %s", err, old)
		}
		if err := removeVerifiedSource(old); err != nil {
			return "", fmt.Errorf("remove %s (already copied to %s): %w", old, newp, err)
		}
		return fmt.Sprintf("moved %s -> %s (copied, verified)", old, newp), nil
	case newDirEmpty:
		if err := os.Remove(newp); err != nil {
			return "", err
		}
	case newDirAbsent:
	default:
		return "", fmt.Errorf("%s already exists and is not a verified copy of %s; refusing to merge or delete either", newp, old)
	}
	// A staging dir next to an intact source is a copy that did not finish
	// (or finished but was not yet renamed): start it over.
	if lexists(staging) {
		if err := checkNotAliased(old, staging); err != nil {
			return "", fmt.Errorf("leftover %s is not a separate copy (%v); refusing to delete it, remove it by hand after checking", staging, err)
		}
		if err := os.RemoveAll(staging); err != nil {
			return "", fmt.Errorf("remove leftover partial copy %s: %w", staging, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(newp), 0o755); err != nil {
		return "", err
	}
	err = ops.Rename(old, newp)
	if err == nil {
		return fmt.Sprintf("moved %s -> %s", old, newp), nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		if errors.Is(err, syscall.EBUSY) {
			return "", fmt.Errorf("%w: %s is a mount point. Unmount it from %s first, mount the volume at %s instead (update /etc/fstab), leave %s empty, then re-run; never mount it at %s while it is still mounted at %s", err, old, old, newp, old, newp, old)
		}
		return "", err
	}
	progressf("%s and %s are on different filesystems; copying...", old, newp)
	files, size, err := copyTreeVerified(old, staging, ops)
	if err != nil {
		if rmErr := os.RemoveAll(staging); rmErr != nil {
			return "", fmt.Errorf("copy %s -> %s: %w (and removing the partial copy failed: %v)", old, staging, err, rmErr)
		}
		return "", fmt.Errorf("copy %s -> %s: %w (partial copy removed; %s is untouched)", old, staging, err, old)
	}
	if err := writeFileSync(filepath.Join(staging, copyVerifiedMarker), []byte(oldTok+"\nverified copy of "+old+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(staging, newp); err != nil {
		return "", fmt.Errorf("rename verified copy %s -> %s: %w", staging, newp, err)
	}
	if err := checkNotAliased(old, newp); err != nil {
		return "", fmt.Errorf("%w; refusing to delete %s", err, old)
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
		// The source's own migration markers are not data: the in-progress
		// marker stays with the source (it goes last when the source is
		// removed) and the copy gets its own copy-verified marker.
		if rel == migratingMarker || rel == copyVerifiedMarker {
			return nil
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
	progressf("verifying %d entries in %s...", len(want), dst)
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
