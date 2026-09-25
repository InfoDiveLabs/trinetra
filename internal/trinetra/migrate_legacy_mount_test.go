package trinetra

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These run only as root where mount(8) works (e.g. a --privileged Linux
// container); elsewhere they skip.

func mountOrSkip(t *testing.T, args ...string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if out, err := exec.Command("mount", args...).CombinedOutput(); err != nil {
		t.Skipf("mount %v: %v %s", args, err, out)
	}
	target := args[len(args)-1]
	t.Cleanup(func() { _ = exec.Command("umount", "-l", target).Run() })
}

// Review repro B: the legacy state dir is a mount point. The first run must
// refuse before touching anything; bind-mounting it at the new path as well
// (what the old hint suggested) must not lead to the shared volume being
// emptied.
func TestLegacyMigrationMountPointAndBindMountAsRoot(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	stash := t.TempDir()
	if out, err := exec.Command("cp", "-a", p.OldStateDir+"/.", stash).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	mountOrSkip(t, "-t", "tmpfs", "tmpfs", p.OldStateDir)
	if out, err := exec.Command("cp", "-a", stash+"/.", p.OldStateDir).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	wantState = snapshot(t, p.OldStateDir) // tmpfs root mode may differ

	f := &fakeMigrationOps{paths: p}
	_, err := runMigration(t, f)
	if err == nil || !strings.Contains(err.Error(), "unmount the volume from "+p.OldStateDir+" first") {
		t.Fatalf("first run: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("systemctl touched before refusing: %v", f.calls)
	}

	if err := os.MkdirAll(p.NewStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mountOrSkip(t, "--bind", p.OldStateDir, p.NewStateDir)
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("re-run with the volume bind-mounted at the new path did not refuse")
	}
	if got := snapshot(t, p.OldStateDir); !equalSnap(got, wantState) {
		t.Fatalf("volume changed\n got %v\nwant %v", got, wantState)
	}
}

// After a crash, a bind mount of the (non-mount-point) old dir at the new
// path carries the in-progress marker: refuse, delete nothing.
func TestLegacyMigrationBindMountedNewDirAsRoot(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	f := &fakeMigrationOps{paths: p, exdev: true, failStep: "move-state"}
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected simulated crash")
	}
	if err := os.MkdirAll(p.NewStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mountOrSkip(t, "--bind", p.OldStateDir, p.NewStateDir)
	f.failStep = ""
	if _, err := runMigration(t, f); err == nil {
		t.Fatal("expected refusal")
	}
	assertLegacyIntact(t, p, wantState)
}

// A same-filesystem bind mount keeps the device number; mountinfo still
// identifies it, and checkNotAliased catches it by device+inode.
func TestSameDeviceBindMountDetectedAsRoot(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	mustWrite(t, filepath.Join(src, "f"), "x")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	mountOrSkip(t, "--bind", src, dst)
	if !isMountPoint(dst) {
		t.Error("same-device bind mount not detected as a mount point")
	}
	if err := checkNotAliased(src, dst); err == nil {
		t.Error("bind mount not detected as an alias")
	}
}

// P4: the real legacy state volume did not mount this boot (an empty mount
// point dir). Refuse without touching anything; once the volume is back, the
// migration moves its data.
func TestLegacyMigrationUnmountedStateVolumeAsRoot(t *testing.T) {
	p := testMigrationPaths(t)
	makeLegacyInstall(t, p)
	wantState := snapshot(t, p.OldStateDir)
	stash := t.TempDir()
	if out, err := exec.Command("cp", "-a", p.OldStateDir+"/.", stash).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := os.RemoveAll(p.OldStateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.OldStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mountOrSkip(t, "-t", "tmpfs", "tmpfs", p.OldStateDir)
	if out, err := exec.Command("umount", p.OldStateDir).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	refuseNothingChanged(t, p, &fakeMigrationOps{paths: p}, "is its volume mounted?")
	if !lexistsT(p.OldStateDir) {
		t.Fatal("mount point dir removed")
	}
	// The volume is back (data restored into the dir): migrate normally.
	if out, err := exec.Command("cp", "-a", stash+"/.", p.OldStateDir).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if _, err := runMigration(t, &fakeMigrationOps{paths: p}); err != nil {
		t.Fatal(err)
	}
	got := snapshot(t, p.NewStateDir)
	delete(got, migratedFromServerwatchMarker)
	if !equalSnap(got, wantState) {
		t.Fatalf("state not migrated intact\n got %v\nwant %v", got, wantState)
	}
}
