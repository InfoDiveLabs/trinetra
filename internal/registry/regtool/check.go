package regtool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// Check validates the plugin source tree at pluginDir: manifest, host_api range and directory name.
func Check(pluginDir string) error {
	raw, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
	if err != nil {
		return err
	}
	m, err := decodeLoose(raw)
	if err != nil {
		return err
	}
	if len(m.Binaries) == 0 {
		m.Binaries = []plugin.Binary{{OS: "linux", Arch: "amd64", SHA256: strings.Repeat("0", 64), Size: 1}}
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if err := plugin.HostAPICompatible(m.HostAPI); err != nil {
		return err
	}
	if _, err := update.ParseVersion(m.Version); err != nil {
		return err
	}
	if m.MinTrinetra != "" {
		if _, err := update.ParseVersion(m.MinTrinetra); err != nil {
			return fmt.Errorf("manifest: min_trinetra: %w", err)
		}
	}
	dir := filepath.Base(filepath.Clean(pluginDir))
	if !strings.HasPrefix(dir, "_") && dir != m.ID {
		return fmt.Errorf("plugin directory %q must be named after the manifest id %q", dir, m.ID)
	}
	return nil
}
