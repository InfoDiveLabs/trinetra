package pluginrt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// ArchiveName is the kept copy of the installed package, used for fleet push.
const ArchiveName = "package.tar.gz"

// Store persists plugin records, config, tokens and installed versions.
type Store struct {
	stateDir string
	libDir   string
	goos     string
	goarch   string
	mu       sync.Mutex
}

// OpenStore creates <stateDir>/plugins (0700) and libDir (0755).
func OpenStore(stateDir, libDir string) (*Store, error) {
	root := filepath.Join(stateDir, "plugins")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return nil, err
	}
	_ = os.Chmod(root, 0o700)
	_ = os.Chmod(libDir, 0o755)
	if stale, _ := filepath.Glob(filepath.Join(libDir, ".stage-*")); len(stale) > 0 {
		for _, d := range stale {
			_ = os.RemoveAll(d)
		}
	}
	return &Store{stateDir: stateDir, libDir: libDir, goos: runtime.GOOS, goarch: runtime.GOARCH}, nil
}

// SetPlatform sets the platform whose binary Install reports.
func (s *Store) SetPlatform(goos, goarch string) { s.goos, s.goarch = goos, goarch }

func (s *Store) root() string { return filepath.Join(s.stateDir, "plugins") }

func (s *Store) dir(id string) (string, error) {
	if !plugin.IDPattern.MatchString(id) {
		return "", fmt.Errorf("plugin id %q is invalid", id)
	}
	return filepath.Join(s.root(), id), nil
}

// Staging returns a fresh directory on the library filesystem for extracting a package, and its cleanup.
func (s *Store) Staging() (dir string, cleanup func(), err error) {
	dir, err = os.MkdirTemp(s.libDir, ".stage-")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// Load returns the record for id; plugin.ErrNotFound when absent.
func (s *Store) Load(id string) (plugin.Installed, error) {
	d, err := s.dir(id)
	if err != nil {
		return plugin.Installed{}, plugin.ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(d, plugin.RecordFileName))
	if errors.Is(err, os.ErrNotExist) {
		return plugin.Installed{}, plugin.ErrNotFound
	}
	if err != nil {
		return plugin.Installed{}, err
	}
	var rec plugin.Installed
	if err := json.Unmarshal(b, &rec); err != nil {
		return plugin.Installed{}, fmt.Errorf("plugin %s: corrupt record: %w", id, err)
	}
	return rec, nil
}

// Save writes rec atomically (0600).
func (s *Store) Save(rec plugin.Installed) error {
	d, err := s.dir(rec.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return update.WriteFileAtomic(filepath.Join(d, plugin.RecordFileName), b, 0o600)
}

// List returns every record sorted by id; unreadable records are skipped.
func (s *Store) List() ([]plugin.Installed, error) {
	ents, err := os.ReadDir(s.root())
	if err != nil {
		return nil, err
	}
	var out []plugin.Installed
	for _, e := range ents {
		if !e.IsDir() || !plugin.IDPattern.MatchString(e.Name()) {
			continue
		}
		rec, err := s.Load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Delete removes the state and library directories of id.
func (s *Store) Delete(id string) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	e1 := os.RemoveAll(d)
	e2 := os.RemoveAll(filepath.Join(s.libDir, id))
	return errors.Join(e1, e2)
}

// ReadConfig returns the stored config; empty when none was written.
func (s *Store) ReadConfig(id string) (plugin.ConfigResponse, error) {
	d, err := s.dir(id)
	if err != nil {
		return plugin.ConfigResponse{}, err
	}
	b, err := os.ReadFile(filepath.Join(d, plugin.ConfigFileName))
	if errors.Is(err, os.ErrNotExist) {
		return plugin.ConfigResponse{Values: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return plugin.ConfigResponse{}, err
	}
	var cfg plugin.ConfigResponse
	if err := json.Unmarshal(b, &cfg); err != nil {
		return plugin.ConfigResponse{}, fmt.Errorf("plugin %s: corrupt config: %w", id, err)
	}
	if cfg.Values == nil {
		cfg.Values = map[string]json.RawMessage{}
	}
	return cfg, nil
}

// WriteConfig stores cfg atomically (0600) with the revision incremented.
func (s *Store) WriteConfig(id string, cfg plugin.ConfigResponse) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.ReadConfig(id)
	if err != nil {
		return err
	}
	cfg.Revision = old.Revision + 1
	if cfg.Values == nil {
		cfg.Values = map[string]json.RawMessage{}
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return update.WriteFileAtomic(filepath.Join(d, plugin.ConfigFileName), b, 0o600)
}

// WriteToken stores the launch token (0600) and returns its path.
func (s *Store) WriteToken(id, token string) (string, error) {
	d, err := s.dir(id)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(d, plugin.TokenFileName)
	return p, update.WriteFileAtomic(p, []byte(token), 0o600)
}

// VersionDir is <libDir>/<id>/<version>.
func (s *Store) VersionDir(id, version string) string {
	return filepath.Join(s.libDir, id, version)
}

// Install moves the extracted package into its version directory and keeps its archive for fleet push.
func (s *Store) Install(p Package, version string) (string, error) {
	id := p.Manifest.ID
	if !plugin.IDPattern.MatchString(id) {
		return "", fmt.Errorf("plugin id %q is invalid", id)
	}
	if _, err := update.ParseVersion(version); err != nil {
		return "", err
	}
	dest := s.VersionDir(id, version)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	_ = os.Chmod(filepath.Dir(dest), 0o755)
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Rename(p.Dir, dest); err != nil {
		return "", err
	}
	if err := os.Chmod(dest, 0o755); err != nil {
		return "", err
	}
	if p.Archive != "" {
		if err := update.CopyFile(p.Archive, filepath.Join(dest, ArchiveName), 0o644); err != nil {
			return "", err
		}
	}
	return filepath.Join(dest, filepath.FromSlash(plugin.BinaryPath(s.goos, s.goarch, id))), nil
}

// PruneVersions removes every installed version of id except keep.
func (s *Store) PruneVersions(id, keep string) error {
	if !plugin.IDPattern.MatchString(id) {
		return fmt.Errorf("plugin id %q is invalid", id)
	}
	base := filepath.Join(s.libDir, id)
	ents, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if e.Name() != keep {
			errs = append(errs, os.RemoveAll(filepath.Join(base, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// AppendAudit appends e to <stateDir>/plugins/audit.jsonl.
func (s *Store) AppendAudit(e plugin.AuditEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.root(), plugin.AuditFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// ReadFile reads a file from the installed version directory, capped at max bytes.
func (s *Store) ReadFile(id, version, rel string, max int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(s.VersionDir(id, version), filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, plugin.ErrTooLarge
	}
	return b, nil
}
