package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

// TestUserStorePutGetRoundTrip pins the core persistence contract: a user (with a
// credential) written via Put comes back byte-for-byte equivalent via Get.
func TestUserStorePutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newUserStore(dir)

	u := &User{
		ID:      "user-1",
		Name:    "on-call",
		Role:    RoleViewer,
		Created: 1_700_000_000,
		Credentials: []Credential{
			{
				ID:         []byte{1, 2, 3, 4},
				PublicKey:  []byte{5, 6, 7, 8, 9},
				SignCount:  0,
				Transports: []string{"internal"},
			},
		},
	}
	if err := store.Put(u); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A fresh store instance over the same directory must see the same data.
	reloaded := newUserStore(dir)
	got, ok := reloaded.Get("user-1")
	if !ok {
		t.Fatal("Get(user-1) = not found, want found")
	}
	if got.Name != u.Name || got.Role != u.Role || got.Created != u.Created {
		t.Errorf("Get(user-1) = %+v, want %+v", got, u)
	}
	if len(got.Credentials) != 1 {
		t.Fatalf("Get(user-1).Credentials len = %d, want 1", len(got.Credentials))
	}
	gc := got.Credentials[0]
	if string(gc.ID) != string(u.Credentials[0].ID) || string(gc.PublicKey) != string(u.Credentials[0].PublicKey) {
		t.Errorf("Get(user-1).Credentials[0] = %+v, want %+v", gc, u.Credentials[0])
	}
	if len(gc.Transports) != 1 || gc.Transports[0] != "internal" {
		t.Errorf("Get(user-1).Credentials[0].Transports = %v, want [internal]", gc.Transports)
	}
}

// TestUserStorePersistsWith0600Perms pins the design doc's security checklist expectation
// for on-disk secrets/credential material: users.json must not be group/world-readable.
func TestUserStorePersistsWith0600Perms(t *testing.T) {
	dir := t.TempDir()
	store := newUserStore(dir)
	if err := store.Put(&User{ID: "u1", Name: "a"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	fi, err := os.Stat(filepath.Join(dir, "users.json"))
	if err != nil {
		t.Fatalf("stat users.json: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("users.json perms = %o, want 0600", got)
	}
}

// TestUserStoreByNameListDelete covers the rest of the UserStore interface:
// looking a user up by name, listing all users, and deleting one.
func TestUserStoreByNameListDelete(t *testing.T) {
	dir := t.TempDir()
	store := newUserStore(dir)

	if err := store.Put(&User{ID: "u1", Name: "alice"}); err != nil {
		t.Fatalf("Put(u1): %v", err)
	}
	if err := store.Put(&User{ID: "u2", Name: "bob"}); err != nil {
		t.Fatalf("Put(u2): %v", err)
	}

	if u, ok := store.ByName("bob"); !ok || u.ID != "u2" {
		t.Errorf("ByName(bob) = %+v, %v, want u2, true", u, ok)
	}
	if _, ok := store.ByName("nobody"); ok {
		t.Error("ByName(nobody) = found, want not found")
	}

	if got := len(store.List()); got != 2 {
		t.Errorf("List() len = %d, want 2", got)
	}

	if err := store.Delete("u1"); err != nil {
		t.Fatalf("Delete(u1): %v", err)
	}
	if _, ok := store.Get("u1"); ok {
		t.Error("Get(u1) after Delete = found, want not found")
	}
	if got := len(store.List()); got != 1 {
		t.Errorf("List() after Delete len = %d, want 1", got)
	}
	if err := store.Delete("u1"); err == nil {
		t.Error("Delete(u1) again = nil error, want error (already deleted)")
	}
}

// TestUserStoreGetMissingIsNotFound pins the empty/nonexistent-file case: a fresh StateDir
// with no users.json yet must behave as "not found", not error out.
func TestUserStoreGetMissingIsNotFound(t *testing.T) {
	store := newUserStore(t.TempDir())
	if _, ok := store.Get("anything"); ok {
		t.Error("Get on empty store = found, want not found")
	}
	if got := store.List(); got != nil {
		t.Errorf("List on empty store = %v, want nil", got)
	}
}

// TestUserImplementsWebauthnUser pins *User's webauthn.User implementation:
// WebAuthnID must return the raw ID bytes (not the name -- go-webauthn's
// interface doc is explicit that identity must key off the handle, not the
// display name), and WebAuthnCredentials must adapt this package's
// flattened Credential slice into go-webauthn's shape without dropping the
// public key, sign count, or transports.
func TestUserImplementsWebauthnUser(t *testing.T) {
	u := &User{
		ID:   "the-id",
		Name: "on-call",
		Credentials: []Credential{
			{ID: []byte{9, 9}, PublicKey: []byte{1, 2, 3}, SignCount: 7, Transports: []string{"usb", "nfc"}},
		},
	}

	var _ webauthn.User = u

	if got, want := string(u.WebAuthnID()), "the-id"; got != want {
		t.Errorf("WebAuthnID() = %q, want %q", got, want)
	}
	if got, want := u.WebAuthnName(), "on-call"; got != want {
		t.Errorf("WebAuthnName() = %q, want %q", got, want)
	}
	if got, want := u.WebAuthnDisplayName(), "on-call"; got != want {
		t.Errorf("WebAuthnDisplayName() = %q, want %q", got, want)
	}

	creds := u.WebAuthnCredentials()
	if len(creds) != 1 {
		t.Fatalf("WebAuthnCredentials() len = %d, want 1", len(creds))
	}
	c := creds[0]
	if string(c.ID) != "\x09\x09" || string(c.PublicKey) != "\x01\x02\x03" {
		t.Errorf("WebAuthnCredentials()[0] ID/PublicKey = %v/%v, want [9 9]/[1 2 3]", c.ID, c.PublicKey)
	}
	if c.Authenticator.SignCount != 7 {
		t.Errorf("WebAuthnCredentials()[0].Authenticator.SignCount = %d, want 7", c.Authenticator.SignCount)
	}
	if len(c.Transport) != 2 || string(c.Transport[0]) != "usb" || string(c.Transport[1]) != "nfc" {
		t.Errorf("WebAuthnCredentials()[0].Transport = %v, want [usb nfc]", c.Transport)
	}
}
