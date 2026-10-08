package control

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// dialRetryInterval is how often DialWait retries while the daemon starts.
const dialRetryInterval = 200 * time.Millisecond

// DialWait is Dial with a grace period for a daemon that is still starting
// (#162). `trinetra install` restarts a Type=simple unit, so systemctl
// returns before the daemon has created its socket, and an operator who
// runs `trinetra cli` straight away would otherwise see a bare "no such
// file or directory". While the socket is missing or refusing connections
// and wait has not elapsed, DialWait retries, calling waiting (if non-nil)
// once before the first retry so the caller can say what is happening.
//
// token is re-read on every attempt: the daemon writes a fresh per-launch
// token at startup, so a token read before the socket existed is stale. A
// failure waiting cannot fix (a token read error, or the server rejecting
// the token it was just given) returns at once, except when the token has
// changed since the attempt, which means the daemon finished starting
// between the read and the handshake.
func DialWait(path string, token func() (string, error), wait time.Duration, waiting func()) (*Client, error) {
	deadline := time.Now().Add(wait)
	notified := false
	for {
		tok, err := token()
		if err != nil {
			return nil, err
		}
		c, err := Dial(path, tok)
		if err == nil {
			return c, nil
		}
		if !daemonStarting(err) {
			now, terr := token()
			if terr != nil || now == tok {
				return nil, err
			}
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, err
		}
		if !notified && waiting != nil {
			waiting()
		}
		notified = true
		time.Sleep(min(dialRetryInterval, left))
	}
}

// daemonStarting reports whether a dial error is what a daemon that has not
// yet created (or begun accepting on) its socket produces.
func daemonStarting(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
