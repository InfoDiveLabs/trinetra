package registry

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// State is the persisted anti-rollback record for the last accepted index.
type State struct {
	LastSerial int64  `json:"last_serial"`
	LastSHA256 string `json:"last_sha256"`
	FetchedAt  int64  `json:"fetched_at"`
	Expires    int64  `json:"expires"`
}

// LoadState reads path; a missing file yields the zero State.
func LoadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// SaveState writes st atomically with mode 0600.
func SaveState(path string, st State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return update.WriteFileAtomic(path, b, 0o600)
}
