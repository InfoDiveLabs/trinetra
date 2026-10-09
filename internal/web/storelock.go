package web

import (
	"os"
	"syscall"
)

// lockStore serializes load-modify-save of a JSON store file within this
// process (fileStoreMutex) and across processes (flock on path.lock): the
// CLI's `users` commands write the same files the web server does.
func lockStore(path string) (unlock func()) {
	mu := fileStoreMutex(path)
	mu.Lock()
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return mu.Unlock
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return mu.Unlock
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		mu.Unlock()
	}
}
