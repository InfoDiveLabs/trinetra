package control

import (
	"errors"
	"os"
	"syscall"
	"time"
)

const dialRetryInterval = 200 * time.Millisecond

// DialWait is Dial that keeps retrying, for up to wait, while the daemon is
// still starting (#162). waiting, if non-nil, runs once before the first
// retry. token is re-read per attempt because the daemon writes a fresh one
// at startup; other errors return at once unless the token changed meanwhile.
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

func daemonStarting(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
