package trinetra

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

var (
	pluginSysExec          Exec = osExec{}
	pluginTemplateUnitPath      = pluginrt.TemplateUnitPath
	pluginDropInGlob            = "/etc/systemd/system/trinetra-plugin@*.service.d"
	pluginLibDirPath            = pluginLibDir
	pluginUserExists            = func() bool { _, err := user.Lookup(pluginSystemUser); return err == nil }
	pluginGroupExists           = func() bool { _, err := user.LookupGroup(pluginSystemUser); return err == nil }
)

// ensurePluginSystem creates group/user trinetra-plugin (system, no home, nologin), the lib dir, and the template unit, then runs daemon-reload; idempotent. /run/trinetra-plugins is tmpfs and is created at every daemon start by hostapi's Service.Listen (H9).
func ensurePluginSystem() error {
	x := pluginSysExec
	if err := ensurePluginUser(); err != nil {
		return err
	}
	if err := os.MkdirAll(pluginLibDirPath, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(pluginLibDirPath, 0o755); err != nil {
		return err
	}
	want := pluginrt.RenderTemplateUnit()
	if cur, err := os.ReadFile(pluginTemplateUnitPath); err == nil && string(cur) == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(pluginTemplateUnitPath), 0o755); err != nil {
		return err
	}
	if err := update.WriteFileAtomic(pluginTemplateUnitPath, []byte(want), 0o644); err != nil {
		return err
	}
	if out, err := x.Run("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensurePluginUser creates the trinetra-plugin system user (and group) if missing, with useradd or, on busybox systems, adduser.
func ensurePluginUser() error {
	if pluginUserExists() {
		return nil
	}
	x := pluginSysExec
	args := []string{"--system", "--no-create-home", "--shell", "/usr/sbin/nologin"}
	if pluginGroupExists() {
		args = append(args, "--gid", pluginSystemUser)
	} else {
		args = append(args, "--user-group")
	}
	args = append(args, pluginSystemUser)
	out, err := x.Run("useradd", args...)
	if err == nil {
		return nil
	}
	out2, err2 := x.Run("adduser", "-S", "-D", "-H", "-s", "/sbin/nologin", pluginSystemUser)
	if err2 == nil {
		return nil
	}
	out = append(out, out2...)
	return fmt.Errorf("create %s user: %v: %s", pluginSystemUser, err, strings.TrimSpace(string(out)))
}

// ensurePluginSystemBestEffort runs ensurePluginSystem as root and only warns on failure, so a plugin-system problem never fails the core install.
func ensurePluginSystemBestEffort() {
	if os.Geteuid() != 0 {
		return
	}
	if err := ensurePluginSystem(); err != nil {
		fmt.Fprintf(stderr, "warning: plugin system setup failed (plugins unavailable): %v\n", err)
	}
}

// removePluginSystem stops trinetra-plugin@*.service units, removes the template unit and drop-ins; keeps the user.
func removePluginSystem() {
	x := pluginSysExec
	_, _ = x.Run("systemctl", "stop", "trinetra-plugin@*.service")
	_ = os.Remove(pluginTemplateUnitPath)
	dirs, _ := filepath.Glob(pluginDropInGlob)
	for _, d := range dirs {
		_ = os.RemoveAll(d)
	}
}
