//go:build linux

package trinetra

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
)

const prSetNoNewPrivs = 38

func runPluginExec(id, binPath, tokenPath string) int {
	if err := pluginExecLaunch(id, binPath, tokenPath); err != nil {
		fmt.Fprintf(stderr, "trinetra plugin-exec: %v\n", err)
		return 1
	}
	return 0
}

func pluginExecLaunch(id, binPath, tokenPath string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root to drop privileges")
	}
	u, err := user.Lookup(pluginSystemUser)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	runDir := filepath.Dir(pluginrt.PluginSocketPath(id))
	dataDir := filepath.Join(pluginDataRoot, id)
	for _, d := range []string{runDir, dataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Dir(d), 0o711); err != nil {
			return err
		}
		if err := os.Chown(d, uid, gid); err != nil {
			return err
		}
	}
	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		return err
	}
	runToken := filepath.Join(runDir, plugin.TokenFileName)
	if err := os.WriteFile(runToken, tok, 0o400); err != nil {
		return err
	}
	if err := os.Chown(runToken, uid, gid); err != nil {
		return err
	}
	rls, err := pluginrt.Rlimits(pluginrt.DefaultLimits)
	if err != nil {
		return err
	}
	for _, r := range rls {
		if err := syscall.Setrlimit(r.Resource, &syscall.Rlimit{Cur: r.Cur, Max: r.Max}); err != nil {
			return fmt.Errorf("setrlimit %d: %w", r.Resource, err)
		}
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", e)
	}
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	if err := syscall.Setuid(uid); err != nil {
		return err
	}
	env := pluginEnv(id, os.Getenv(plugin.EnvHostSocket), runToken, dataDir)
	return syscall.Exec(binPath, []string{binPath}, env)
}
