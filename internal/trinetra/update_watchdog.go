// Package trinetra: update_watchdog.go installs the persistent self-update
// watchdog (R14): trinetra-update-watchdog.timer fires 2 minutes after boot
// and every minute after, running trinetra-update-watchdog.service, a
// oneshot that executes the pinned guard binary with `update guard
// --if-pending`. It is a no-op when nothing is pending or a guard already
// holds guard.lock; otherwise it resolves the pending update. Recovery
// therefore never depends on the new build starting, and survives a killed
// guard, a crash mid-swap and a reboot.
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

// renderWatchdogService is the oneshot the timer runs. TimeoutStartSec
// bounds a wedged guard (a full gate is about two minutes).
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

// renderWatchdogTimer fires the watchdog 2 minutes after boot, every minute
// after the previous run finished, and shortly (1 minute) after the timer
// itself is (re)started -- e.g. by an uninstall+reinstall within the same
// boot, when OnBootSec has already elapsed and would otherwise wait a full
// boot cycle to fire again. Persistent= is deliberately not set: it only
// applies to OnCalendar= timers and is a no-op (some systemd versions warn
// about it) on a monotonic timer like this one; OnActiveSec= covers the
// same "don't miss a run" concern here.
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

// ensureWatchdog makes the watchdog present and running: it writes the two
// unit files under p.UnitDir when missing or different (daemon-reload only
// then) and enables and starts the timer. install calls it after writing the
// pinned guard; apply calls it before swapping anything, so a host installed
// by an older build still gets its safety net before the first update. The
// pinned guard binary itself is written separately (writePinnedGuard).
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
