package web

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSessionStoreConcurrentNewNoLostUpdate is the shared-per-path-lock
// regression pin for sessions. Each handler builds a FRESH jsonSessionStore
// per request (newSessionStore), so a per-INSTANCE mutex would serialize
// nothing across concurrent requests: two New calls would each load the whole
// file, append their own session, and save it back — last-writer-wins — losing
// one session and, worse, colliding on the shared "sessions.json.tmp" temp
// path. In production this is a concurrent login racing another login/logout/GC
// clobbering a just-created session → intermittent auth failures. With the
// process-wide per-path lock (fileStoreMutex), both New calls serialize and
// both sessions must survive.
func TestSessionStoreConcurrentNewNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	const n = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// A fresh store per goroutine, mirroring the per-request handler
			// pattern — the shared lock must be keyed on the file, not the
			// instance.
			sess, err := newSessionStore(dir).New("user", time.Hour)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = sess.ID
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent New[%d]: %v", i, err)
		}
	}
	// Every created session must still be retrievable — none lost to a
	// last-writer-wins clobber.
	final := newSessionStore(dir)
	for i, id := range ids {
		if id == "" {
			t.Fatalf("New[%d] returned an empty session ID", i)
		}
		if _, ok := final.Get(id); !ok {
			t.Errorf("session %d (%s) was lost — clobbered by a concurrent write", i, id)
		}
	}
}

// TestSessionStoreRoundTripsWith0600Perms pins the basic New/Get contract
// and the on-disk file's permissions: sessions.json holds bearer-equivalent
// session IDs and CSRF tokens, so it must never be group/world-readable.
func TestSessionStoreRoundTripsWith0600Perms(t *testing.T) {
	dir := t.TempDir()
	store := newSessionStore(dir)

	sess, err := store.New("user-1", time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("New returned a session with an empty ID")
	}
	if sess.CSRF == "" {
		t.Error("New returned a session with an empty CSRF token")
	}
	if sess.UserID != "user-1" {
		t.Errorf("UserID = %q, want user-1", sess.UserID)
	}

	got, ok := store.Get(sess.ID)
	if !ok {
		t.Fatal("Get(sess.ID) = not found, want found")
	}
	if got.CSRF != sess.CSRF {
		t.Errorf("Get CSRF = %q, want %q", got.CSRF, sess.CSRF)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "sessions.json"))
		if err != nil {
			t.Fatalf("stat sessions.json: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("sessions.json perm = %o, want 0600", perm)
		}
	}
}

// TestSessionGetTreatsExpiredAsAbsent pins expiry: once the store's clock
// passes a session's Expires, Get must report it not found even though GC
// hasn't run yet (lazy expiry, decoupled from the GC ticker's timing).
func TestSessionGetTreatsExpiredAsAbsent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &jsonSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), now: func() time.Time { return now }}

	sess, err := store.New("user-1", time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	now = now.Add(30 * time.Second)
	if _, ok := store.Get(sess.ID); !ok {
		t.Fatal("Get within TTL = not found, want found")
	}

	now = now.Add(time.Minute)
	if _, ok := store.Get(sess.ID); ok {
		t.Fatal("Get after TTL = found, want expired/absent")
	}
}

// TestSessionGCRemovesExpiredRecords pins that GC actually rewrites the file
// without expired entries, distinct from Get's lazy (non-mutating) check.
func TestSessionGCRemovesExpiredRecords(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &jsonSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), now: func() time.Time { return now }}

	expired, err := store.New("stale", time.Second)
	if err != nil {
		t.Fatalf("New expired: %v", err)
	}
	now = now.Add(time.Hour)
	live, err := store.New("fresh", time.Hour)
	if err != nil {
		t.Fatalf("New live: %v", err)
	}

	store.GC(now.Unix())

	if _, ok := store.Get(live.ID); !ok {
		t.Error("live session missing after GC")
	}
	sessions, err := store.loadLocked()
	if err != nil {
		t.Fatalf("loadLocked: %v", err)
	}
	for _, s := range sessions {
		if s.ID == expired.ID {
			t.Errorf("GC left expired session %q on disk", expired.ID)
		}
	}
}

// TestSessionPutUpdatesData pins that Put persists a mutation (the ceremony
// stash's use of Session.Data) back to the same record New created.
func TestSessionPutUpdatesData(t *testing.T) {
	store := newSessionStore(t.TempDir())
	sess, err := store.New("", 5*time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sess.Data = []byte(`{"hello":"world"}`)
	if err := store.Put(sess); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok := store.Get(sess.ID)
	if !ok {
		t.Fatal("Get after Put = not found")
	}
	if string(got.Data) != `{"hello":"world"}` {
		t.Errorf("Data = %s, want {\"hello\":\"world\"}", got.Data)
	}
}

// TestSessionDeleteIsIdempotent pins that Delete on an absent ID is a no-op,
// not an error — logout and one-shot ceremony consumption both rely on this.
func TestSessionDeleteIsIdempotent(t *testing.T) {
	store := newSessionStore(t.TempDir())
	if err := store.Delete("does-not-exist"); err != nil {
		t.Errorf("Delete(absent) = %v, want nil", err)
	}

	sess, err := store.New("user-1", time.Hour)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.Delete(sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := store.Get(sess.ID); ok {
		t.Error("session still present after Delete")
	}
	if err := store.Delete(sess.ID); err != nil {
		t.Errorf("second Delete = %v, want nil (idempotent)", err)
	}
}

// TestSessionNewRefusesWhenFull pins the pre-auth DoS bound (mirrors Task
// 4's ceremonyStash.put): once the store is at sessionMaxEntries live
// records, New refuses to mint another rather than growing without bound.
func TestSessionNewRefusesWhenFull(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// A small maxEntries override keeps this test's O(n) read-modify-write
	// disk round trips (jsonSessionStore doesn't cache between calls) fast
	// while still exercising the exact same refusal path production hits at
	// the real sessionMaxEntries.
	const testCap = 20
	store := &jsonSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), now: func() time.Time { return now }, maxEntries: testCap}

	for i := 0; i < testCap; i++ {
		if _, err := store.New("u", time.Hour); err != nil {
			t.Fatalf("New %d: %v", i, err)
		}
	}
	if _, err := store.New("overflow", time.Hour); err == nil {
		t.Fatal("New at capacity = nil error, want refusal")
	}
}
