package pluginrt

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// Verify establishes trust and checks the binary for goos/goarch against the signed manifest.
func Verify(p Package, reg plugin.Registry, goos, goarch, coreVersion string) (plugin.Trust, error) {
	m := p.Manifest
	if err := plugin.HostAPICompatible(m.HostAPI); err != nil {
		if errors.Is(err, plugin.ErrIncompatible) {
			return "", err
		}
		return "", fmt.Errorf("%w: %v", plugin.ErrIncompatible, err)
	}
	if m.MinTrinetra != "" && coreVersion != "" && coreVersion != "dev" {
		need, err := update.ParseVersion(m.MinTrinetra)
		if err != nil {
			return "", fmt.Errorf("min_trinetra: %w", err)
		}
		if have, err := update.ParseVersion(coreVersion); err == nil && update.CompareVersions(have, need) < 0 {
			return "", fmt.Errorf("plugin %s needs trinetra %s or newer (this is %s)", m.ID, m.MinTrinetra, coreVersion)
		}
	}
	if reg != nil {
		if reason, revoked := reg.Revocation(m.ID, m.Version); revoked {
			return "", fmt.Errorf("%w: %s", plugin.ErrRevoked, reason)
		}
	}
	trust := plugin.TrustUnverified
	if len(p.Sig) > 0 && reg != nil && reg.VerifyManifest(p.ManifestBytes, p.Sig) == nil {
		trust = plugin.TrustVerified
	}
	if err := checkBinary(p, goos, goarch); err != nil {
		return "", err
	}
	return trust, nil
}

func checkBinary(p Package, goos, goarch string) error {
	m := p.Manifest
	b, ok := m.BinaryFor(goos, goarch)
	if !ok {
		return fmt.Errorf("plugin %s has no binary for %s/%s", m.ID, goos, goarch)
	}
	f, err := os.Open(filepath.Join(p.Dir, filepath.FromSlash(plugin.BinaryPath(goos, goarch, m.ID))))
	if err != nil {
		return fmt.Errorf("plugin %s: binary for %s/%s is missing from the package", m.ID, goos, goarch)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != b.Size || hex.EncodeToString(h.Sum(nil)) != b.SHA256 {
		return fmt.Errorf("plugin %s: binary does not match the manifest (size or sha256 mismatch)", m.ID)
	}
	return nil
}
