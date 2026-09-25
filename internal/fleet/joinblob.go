package fleet

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

const joinPrefix = "swj1_"

// JoinInfo is everything a child needs to enroll: where the master is, the
// one-time token, and the CA pin that authenticates the master before the
// token is sent.
type JoinInfo struct {
	URL   string `json:"u"`
	Token string `json:"t"`
	Pin   string `json:"p"`
}

// EncodeJoin packs j into one copy-pasteable, shell-safe string.
func EncodeJoin(j JoinInfo) string {
	b, _ := json.Marshal(j)
	return joinPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeJoin reverses EncodeJoin and validates each field.
func DecodeJoin(s string) (JoinInfo, error) {
	var j JoinInfo
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, joinPrefix) {
		return j, errors.New("fleet: join code must start with swj1_")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, joinPrefix))
	if err != nil {
		return j, errors.New("fleet: join code is corrupted (copy the whole line)")
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, errors.New("fleet: join code is corrupted (copy the whole line)")
	}
	u, err := url.Parse(j.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return j, errors.New("fleet: join code has no valid https master URL")
	}
	if !strings.HasPrefix(j.Token, tokenPrefix) {
		return j, errors.New("fleet: join code has no valid token")
	}
	if !strings.HasPrefix(j.Pin, "sha256:") {
		return j, errors.New("fleet: join code has no valid CA pin")
	}
	return j, nil
}
