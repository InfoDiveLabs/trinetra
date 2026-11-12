package trinetra

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
)

func pluginSystemSandbox(t *testing.T) (*recExec, string) {
	t.Helper()
	dir := t.TempDir()
	prevX, prevT, prevG, prevL, prevU, prevGr := pluginSysExec, pluginTemplateUnitPath, pluginDropInGlob, pluginLibDirPath, pluginUserExists, pluginGroupExists
	t.Cleanup(func() {
		pluginSysExec, pluginTemplateUnitPath, pluginDropInGlob, pluginLibDirPath, pluginUserExists, pluginGroupExists = prevX, prevT, prevG, prevL, prevU, prevGr
	})
	rx := &recExec{}
	pluginSysExec = rx
	pluginTemplateUnitPath = filepath.Join(dir, "sys", "trinetra-plugin@.service")
	pluginDropInGlob = filepath.Join(dir, "sys", "trinetra-plugin@*.service.d")
	pluginLibDirPath = filepath.Join(dir, "lib")
	pluginUserExists = func() bool { return false }
	pluginGroupExists = func() bool { return false }
	return rx, dir
}

func TestPluginSystemCreatesUserGroupAndUnit(t *testing.T) {
	rx, _ := pluginSystemSandbox(t)
	if err := ensurePluginSystem(); err != nil {
		t.Fatal(err)
	}
	if !rx.ran("useradd --system --no-create-home --shell /usr/sbin/nologin --user-group trinetra-plugin") {
		t.Fatalf("calls = %v", rx.calls)
	}
	if !rx.ran("systemctl daemon-reload") {
		t.Fatalf("no daemon-reload: %v", rx.calls)
	}
	b, err := os.ReadFile(pluginTemplateUnitPath)
	if err != nil || string(b) != pluginrt.RenderTemplateUnit() {
		t.Fatalf("unit: %v", err)
	}
	if st, _ := os.Stat(pluginLibDirPath); st == nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("lib dir: %v", st)
	}
}

func TestPluginSystemSkipsUserWhenPresent(t *testing.T) {
	rx, _ := pluginSystemSandbox(t)
	pluginUserExists = func() bool { return true }
	if err := ensurePluginSystem(); err != nil {
		t.Fatal(err)
	}
	if rx.ran("useradd") {
		t.Fatalf("useradd ran: %v", rx.calls)
	}
}

func TestPluginSystemReusesExistingGroup(t *testing.T) {
	rx, _ := pluginSystemSandbox(t)
	pluginGroupExists = func() bool { return true }
	if err := ensurePluginSystem(); err != nil {
		t.Fatal(err)
	}
	if !rx.ran("--gid trinetra-plugin trinetra-plugin") || rx.ran("--user-group") {
		t.Fatalf("calls = %v", rx.calls)
	}
}

func TestPluginSystemIdempotent(t *testing.T) {
	rx, _ := pluginSystemSandbox(t)
	if err := ensurePluginSystem(); err != nil {
		t.Fatal(err)
	}
	pluginUserExists = func() bool { return true }
	rx.calls = nil
	if err := ensurePluginSystem(); err != nil {
		t.Fatal(err)
	}
	if len(rx.calls) != 0 {
		t.Fatalf("second run executed %v", rx.calls)
	}
}

func TestPluginSystemRemove(t *testing.T) {
	rx, dir := pluginSystemSandbox(t)
	if err := ensurePluginSystem(); err != nil {
		t.Fatal(err)
	}
	dropin := filepath.Join(dir, "sys", "trinetra-plugin@demo.service.d")
	if err := os.MkdirAll(dropin, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dropin, "50-trinetra.conf"), []byte("x"), 0o644)
	rx.calls = nil
	removePluginSystem()
	if !rx.ran("systemctl stop trinetra-plugin@*.service") || rx.ran("userdel") {
		t.Fatalf("calls = %v", rx.calls)
	}
	if _, err := os.Stat(pluginTemplateUnitPath); !os.IsNotExist(err) {
		t.Fatal("template unit remains")
	}
	if _, err := os.Stat(dropin); !os.IsNotExist(err) {
		t.Fatal("drop-in remains")
	}
}

func TestPluginExecArgs(t *testing.T) {
	prev := stderr
	var sb strings.Builder
	stderr = &sb
	t.Cleanup(func() { stderr = prev })
	for _, args := range [][]string{nil, {"a"}, {"demo", "/bin"}, {"demo", "/bin", "/tok", "extra"}, {"Bad_ID", "/bin", "/tok"}, {"../x", "/bin", "/tok"}} {
		if code := cmdPluginExec(args); code != 2 {
			t.Errorf("%v exit %d, want 2", args, code)
		}
	}
}

func TestPluginExecOtherPlatformsRefuse(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("linux has a real launcher")
	}
	prev := stderr
	var sb strings.Builder
	stderr = &sb
	t.Cleanup(func() { stderr = prev })
	if code := cmdPluginExec([]string{"demo", "/bin/true", "/tmp/tok"}); code != 1 || !strings.Contains(sb.String(), "only available on linux") {
		t.Fatalf("code %d msg %q", code, sb.String())
	}
}
