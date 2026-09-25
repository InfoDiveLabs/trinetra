package fleet

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"
)

const tokenPrefix = "swt_"

// ErrTokenInvalid is returned for any bad token without saying which check
// failed, so a caller learns nothing useful by probing.
var ErrTokenInvalid = errors.New("fleet: join token invalid, expired or used up")

var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// ValidTag reports whether t is an acceptable node tag.
func ValidTag(t string) bool { return tagRe.MatchString(t) }

// Token is a stored join token. Only the SHA-256 of the plaintext is kept.
type Token struct {
	ID      string   `json:"id"`
	Hash    string   `json:"hash"`
	Expires int64    `json:"expires"`
	Uses    int      `json:"uses"`
	Tags    []string `json:"tags,omitempty"`
	Created int64    `json:"created"`
	Creator string   `json:"creator,omitempty"`
}

// TokenStore persists join tokens to one JSON file.
type TokenStore struct {
	path string
	mu   sync.Mutex
	toks []Token
}

// OpenTokens loads path (missing file = empty store).
func OpenTokens(path string) (*TokenStore, error) {
	s := &TokenStore{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.toks); err != nil {
		return nil, fmt.Errorf("fleet: parse %s: %w", path, err)
	}
	return s, nil
}

func (s *TokenStore) saveLocked() error {
	b, err := json.MarshalIndent(s.toks, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Create mints a token valid for ttl and uses joins, returning the plaintext
// (shown once) and the stored record.
func (s *TokenStore) Create(ttl time.Duration, uses int, tags []string, creator string, now time.Time) (string, Token, error) {
	if ttl <= 0 {
		return "", Token{}, errors.New("fleet: token ttl must be positive")
	}
	if uses < 1 {
		return "", Token{}, errors.New("fleet: token uses must be >= 1")
	}
	for _, t := range tags {
		if !ValidTag(t) {
			return "", Token{}, fmt.Errorf("fleet: invalid tag %q (lowercase letters, digits, _ . -; max 32)", t)
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Token{}, err
	}
	plain := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	h := hashToken(plain)
	tok := Token{ID: h[:8], Hash: h, Expires: now.Add(ttl).Unix(), Uses: uses, Tags: tags, Created: now.Unix(), Creator: creator}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toks = append(s.toks, tok)
	if err := s.saveLocked(); err != nil {
		s.toks = s.toks[:len(s.toks)-1]
		return "", Token{}, err
	}
	return plain, tok, nil
}

// Consume validates plain and spends one use. Every stored hash is compared
// in constant time so timing does not reveal a near match.
func (s *TokenStore) Consume(plain string, now time.Time) (Token, error) {
	h := []byte(hashToken(plain))
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.toks {
		if subtle.ConstantTimeCompare(h, []byte(s.toks[i].Hash)) == 1 {
			idx = i
		}
	}
	if idx < 0 || s.toks[idx].Expires <= now.Unix() || s.toks[idx].Uses < 1 {
		return Token{}, ErrTokenInvalid
	}
	tok := s.toks[idx]
	s.toks[idx].Uses--
	if s.toks[idx].Uses == 0 {
		s.toks = append(s.toks[:idx], s.toks[idx+1:]...)
	}
	if err := s.saveLocked(); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// List returns unexpired tokens (hash included; callers must not display it).
func (s *TokenStore) List(now time.Time) []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Token
	for _, t := range s.toks {
		if t.Expires > now.Unix() {
			out = append(out, t)
		}
	}
	return out
}

// Delete removes the token with id.
func (s *TokenStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.toks {
		if s.toks[i].ID == id {
			s.toks = append(s.toks[:i], s.toks[i+1:]...)
			return s.saveLocked()
		}
	}
	return fmt.Errorf("fleet: no token %q", id)
}
