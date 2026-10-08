package control

import (
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A daemon that is still starting has not created its socket yet: DialWait
// keeps retrying, says once that it is waiting, and connects as soon as the
// socket appears (#162).
func TestDialWaitConnectsOnceSocketAppears(t *testing.T) {
	path := shortSocketPath(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Errorf("net.Listen: %v", err)
			return
		}
		t.Cleanup(func() { ln.Close() })
		go Serve(&fakeAPI{}, ln, "tok")
	}()

	var waits, tokenReads int32
	token := func() (string, error) {
		atomic.AddInt32(&tokenReads, 1)
		return "tok", nil
	}
	c, err := DialWait(path, token, 5*time.Second, func() { atomic.AddInt32(&waits, 1) })
	if err != nil {
		t.Fatalf("DialWait: %v", err)
	}
	defer c.Close()
	if got := atomic.LoadInt32(&waits); got != 1 {
		t.Errorf("waiting callback ran %d times, want exactly 1", got)
	}
	if got := atomic.LoadInt32(&tokenReads); got < 2 {
		t.Errorf("token read %d times, want it re-read on every attempt", got)
	}
}

// A socket that is already up connects on the first try, with no waiting
// notice.
func TestDialWaitImmediateWhenUp(t *testing.T) {
	path := startTestServer(t, &fakeAPI{}, "")
	waited := false
	c, err := DialWait(path, func() (string, error) { return "", nil }, time.Second, func() { waited = true })
	if err != nil {
		t.Fatalf("DialWait: %v", err)
	}
	c.Close()
	if waited {
		t.Error("waiting callback ran although the socket was up")
	}
}

// When the daemon never comes up, DialWait gives up after the grace period
// with the underlying "no such file" error.
func TestDialWaitGivesUpAfterGrace(t *testing.T) {
	path := shortSocketPath(t)
	start := time.Now()
	_, err := DialWait(path, func() (string, error) { return "", nil }, 400*time.Millisecond, nil)
	if err == nil {
		t.Fatal("DialWait succeeded with no socket")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want it to wrap os.ErrNotExist", err)
	}
	if el := time.Since(start); el < 400*time.Millisecond || el > 3*time.Second {
		t.Errorf("gave up after %v, want about the 400ms grace period", el)
	}
}

// Errors that waiting cannot fix (a rejected token, an unreadable token
// file) fail immediately rather than burning the grace period.
func TestDialWaitDoesNotRetryAuthFailure(t *testing.T) {
	path := startTestServer(t, &fakeAPI{}, "right")
	start := time.Now()
	if _, err := DialWait(path, func() (string, error) { return "wrong", nil }, 5*time.Second, nil); err == nil {
		t.Fatal("DialWait succeeded with a wrong token")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("auth failure took %v, want an immediate failure", el)
	}

	tokenErr := errors.New("permission denied")
	if _, err := DialWait(path, func() (string, error) { return "", tokenErr }, 5*time.Second, nil); !errors.Is(err, tokenErr) {
		t.Errorf("err = %v, want the token read error", err)
	}
}
