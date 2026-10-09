package update

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// TryLock takes an exclusive, non-blocking flock on path (created 0600, its directory 0700
// if missing). ok is false, with a nil error, when another process.
func TryLock(path string) (unlock func(), ok bool, err error) {
	return lock(path, syscall.LOCK_EX|syscall.LOCK_NB)
}

// Lock is TryLock's blocking form, for short critical sections.
func Lock(path string) (unlock func(), err error) {
	unlock, _, err = lock(path, syscall.LOCK_EX)
	return unlock, err
}

func lock(path string, how int) (func(), bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, false, nil
	}
	if err != nil {
		f.Close()
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}

const stateLockFile = "state.lock"

// WithState runs fn on the current state under dir/state.lock and saves the result when fn
// returns nil (fn's error is returned unchanged, with nothing written).
func WithState(dir string, fn func(*State) error) error {
	unlock, err := Lock(filepath.Join(dir, stateLockFile))
	if err != nil {
		return err
	}
	defer unlock()
	st, err := LoadState(dir)
	if err != nil {
		return err
	}
	if err := fn(&st); err != nil {
		return err
	}
	return SaveState(dir, st)
}
