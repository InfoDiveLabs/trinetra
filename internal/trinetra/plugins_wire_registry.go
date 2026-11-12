package trinetra

import (
	"context"
	"path/filepath"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/registry"
)

func wireRegistry(d *pluginDeps) {
	if d.getCfg == nil {
		return
	}
	now := d.now
	if now == nil {
		now = time.Now
	}
	r := registry.New(registry.Options{
		Dir:       filepath.Join(d.stateDir, "plugins", "registry"),
		BaseURL:   func() string { return d.getCfg().PluginsRegistryURL() },
		Keys:      registry.Keys(),
		Now:       now,
		Installed: func() map[string]string { return installedVersions(d) },
		Logf:      d.logf,
	})
	d.registry = r
	ctx, cancel := context.WithCancel(context.Background())
	d.starts = append(d.starts, func() { go r.Run(ctx) })
	d.stops = append(d.stops, cancel)
}

func installedVersions(d *pluginDeps) map[string]string {
	out := map[string]string{}
	switch {
	case d.lifecycle != nil:
		list, err := d.lifecycle.List()
		if err != nil {
			return out
		}
		for _, s := range list {
			out[s.ID] = s.Version
		}
	case d.inventory != nil:
		for _, in := range d.inventory.Enabled() {
			out[in.ID] = in.Version
		}
	}
	return out
}
