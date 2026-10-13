// Package trinetra: update_watchdog.go installs the persistent self-update watchdog:
// trinetra-update-watchdog.timer fires 2 minutes after boot and every minute after.
package trinetra

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

const (
	watchdogServiceName = "trinetra-update-watchdog.service"
	watchdogTimerName   = "trinetra-update-watchdog.timer"
)

// renderWatchdogService is the oneshot the timer runs.
func renderWatchdogService(guardBin string) string {
	return fmt.Sprintf(`[Unit]
Description=Trinetra self-update watchdog (confirms or rolls back a pending update)
After=network-online.target

[Service]
Type=oneshot
ExecStart=%s update guard --if-pending
TimeoutStartSec=15min
StandardOutput=journal
StandardError=journal
`, guardBin)
}

// renderWatchdogTimer fires the watchdog 2 minutes after boot, every minute after the
// previous run finished, and shortly (1 minute) after the timer itself is (re)started.
func renderWatchdogTimer() string {
	return `[Unit]
Description=Run the Trinetra self-update watchdog every minute

[Timer]
OnBootSec=2min
OnUnitActiveSec=1min
OnActiveSec=1min
AccuracySec=5s

[Install]
WantedBy=timers.target
`
}

// ensureWatchdog makes the watchdog present and running: it writes the two unit files under
// p.UnitDir when missing or different.
func ensureWatchdog(p updatePaths, x Exec) error {
	if p.UnitDir == "" {
		return fmt.Errorf("update: no systemd unit directory configured")
	}
	changed := false
	for name, body := range map[string]string{
		watchdogServiceName: renderWatchdogService(p.guardBin()),
		watchdogTimerName:   renderWatchdogTimer(),
	} {
		path := filepath.Join(p.UnitDir, name)
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, []byte(body)) {
			continue
		}
		if err := update.WriteFileAtomic(path, []byte(body), 0o644); err != nil {
			return fmt.Errorf("update watchdog: write %s: %w", path, err)
		}
		changed = true
	}
	if changed {
		if out, err := x.Run("systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("update watchdog: systemctl daemon-reload: %v %s", err, out)
		}
	}
	if out, err := x.Run("systemctl", "enable", "--now", watchdogTimerName); err != nil {
		return fmt.Errorf("update watchdog: systemctl enable --now %s: %v %s", watchdogTimerName, err, out)
	}
	return nil
}

// removeWatchdog is uninstall's best-effort counterpart.
func removeWatchdog(p updatePaths, x Exec) {
	_, _ = x.Run("systemctl", "disable", "--now", watchdogTimerName)
	_ = os.Remove(filepath.Join(p.UnitDir, watchdogTimerName))
	_ = os.Remove(filepath.Join(p.UnitDir, watchdogServiceName))
	_ = os.RemoveAll(p.GuardDir)
	_ = os.Remove(filepath.Dir(p.GuardDir)) // /usr/local/lib/trinetra, only if now empty
}
