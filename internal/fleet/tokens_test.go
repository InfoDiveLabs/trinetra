package fleet

import (
	"errors"
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
