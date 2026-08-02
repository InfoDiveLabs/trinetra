// Package serverwatch: enroll.go holds the shared Telegram enrollment-pin
// state (#90). Before this, the enrollment pin was a pollLoop-local
// variable (daemon.go) known only inside that goroutine, so `serverwatch
// telegram set-token` -- a separate process -- had no way to show the user
// the pin the daemon would actually accept in "/start <pin>". enrollState
// is constructed once in cmdDaemon and handed to BOTH pollLoop and
// newInprocAPI, so a socket caller's EnrollmentPIN (core.API) always
// returns the exact pin the poll loop is matching against.
package serverwatch

import (
	"sync"

	"serverwatch/internal/config"
)

// enrollState holds the current Telegram enrollment PIN. The daemon's poll
// loop and the control socket's EnrollmentPIN both read it, so `set-token`
// (or ctl) can display the exact pin the daemon will accept in /start <pin>.
// The zero value is ready to use.
type enrollState struct {
	mu  sync.Mutex
	pin string
}

// PIN returns the active enrollment pin for cfg: if telegram is configured
// (token set) AND not yet enrolled (chat id empty), it returns the pin,
// generating and caching it on first call so repeated calls against the
// same not-yet-enrolled config keep returning the identical value. Once
// enrolled (chat id set) it returns "" and enrolled=true; if telegram isn't
// configured at all it returns "" and enrolled=false -- there is nothing to
// enroll into yet. A nil cfg is treated the same as "not configured".
func (e *enrollState) PIN(cfg *config.Config) (pin string, enrolled bool) {
	if cfg == nil {
		return "", false
	}
	if cfg.Telegram.ChatID != "" {
		return "", true
	}
	if cfg.Telegram.Token == "" {
		return "", false
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pin == "" {
		e.pin = newEnrollPIN()
	}
	return e.pin, false
}

// Reset clears the cached pin. Call it once enrollment completes (the chat
// id becomes set), so a later re-enrollment (e.g. after `telegram
// set-token` swaps to a new bot) starts from a fresh pin rather than
// reusing one that was already consumed.
func (e *enrollState) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pin = ""
}
