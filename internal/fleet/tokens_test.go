package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenCreateConsumeSingleUse(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens.json")
	s, err := OpenTokens(p)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	plain, tok, err := s.Create(time.Hour, 1, []string{"prod", "db"}, "admin", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "swt_") || strings.Contains(tok.Hash, plain) {
		t.Fatalf("plain=%q hash=%q", plain, tok.Hash)
	}
	assertMode(t, p, 0o600)

	got, err := s.Consume(plain, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Tags, ",") != "prod,db" {
		t.Fatalf("tags = %v", got.Tags)
	}
	if _, err := s.Consume(plain, now.Add(2*time.Minute)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("second use err = %v", err)
	}
}

func TestTokenExpiresAndPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens.json")
	s, _ := OpenTokens(p)
	now := time.Unix(1_000_000, 0)
	plain, _, err := s.Create(time.Hour, 3, nil, "admin", now)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := OpenTokens(p) // reopen from disk
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Consume(plain, now.Add(2*time.Hour)); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("expired token err = %v", err)
	}
	if n := len(s2.List(now)); n != 1 {
		t.Fatalf("List before expiry = %d", n)
	}
	if n := len(s2.List(now.Add(2 * time.Hour))); n != 0 {
		t.Fatalf("List after expiry = %d", n)
	}
}

func TestTokenMultiUseAndDelete(t *testing.T) {
	s, _ := OpenTokens(filepath.Join(t.TempDir(), "tokens.json"))
	now := time.Unix(1_000_000, 0)
	plain, tok, _ := s.Create(time.Hour, 2, nil, "admin", now)
	if _, err := s.Consume(plain, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(plain, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("deleted token err = %v", err)
	}
	if _, err := s.Consume("swt_wrong", now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("wrong token err = %v", err)
	}
}

func TestTokenCreateValidates(t *testing.T) {
	s, _ := OpenTokens(filepath.Join(t.TempDir(), "tokens.json"))
	now := time.Now()
	if _, _, err := s.Create(0, 1, nil, "a", now); err == nil {
		t.Fatal("zero ttl accepted")
	}
	if _, _, err := s.Create(time.Hour, 0, nil, "a", now); err == nil {
		t.Fatal("zero uses accepted")
	}
	if _, _, err := s.Create(time.Hour, 1, []string{"bad tag!"}, "a", now); err == nil {
		t.Fatal("bad tag accepted")
	}
}

func TestTokenConsumeRollsBackOnSaveFailure(t *testing.T) {
	// Skip if running as root (can't make directories read-only)
	if os.Geteuid() == 0 {
		t.Skip("skipping as root")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "tokens.json")
	s, _ := OpenTokens(p)
	now := time.Unix(1_000_000, 0)
	plain, _, _ := s.Create(time.Hour, 2, nil, "admin", now)

	// Make directory read-only to force save failure
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	defer os.Chmod(dir, 0o700) // restore for cleanup

	// Attempt to consume should fail
	_, err := s.Consume(plain, now)
	if err == nil {
		t.Fatal("Consume succeeded when save should have failed")
	}

	// Restore directory permissions
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore chmod failed: %v", err)
	}

	// Token should still be consumable (2 uses, only 0 decremented in-memory before rollback)
	tok, err := s.Consume(plain, now)
	if err != nil {
		t.Fatalf("Consume after rollback failed: %v", err)
	}
	if tok.Uses != 2 { // Should be original value since mutation was rolled back
		t.Fatalf("token uses after rollback = %d, want 2", tok.Uses)
	}
}

func TestTokenDeleteRollsBackOnSaveFailure(t *testing.T) {
	// Skip if running as root (can't make directories read-only)
	if os.Geteuid() == 0 {
		t.Skip("skipping as root")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "tokens.json")
	s, _ := OpenTokens(p)
	now := time.Unix(1_000_000, 0)
	_, tok, _ := s.Create(time.Hour, 1, nil, "admin", now)

	// Make directory read-only to force save failure
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	defer os.Chmod(dir, 0o700) // restore for cleanup

	// Attempt to delete should fail
	err := s.Delete(tok.ID)
	if err == nil {
		t.Fatal("Delete succeeded when save should have failed")
	}

	// Restore directory permissions
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("restore chmod failed: %v", err)
	}

	// Token should still be in the list (deletion was rolled back)
	list := s.List(now)
	if len(list) != 1 {
		t.Fatalf("List after rollback has %d tokens, want 1", len(list))
	}
	if list[0].ID != tok.ID {
		t.Fatalf("token ID mismatch after rollback")
	}
}
