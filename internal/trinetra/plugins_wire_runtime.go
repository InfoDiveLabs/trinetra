package trinetra

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
)

// wireRuntime builds the plugin lifecycle manager and its process supervisor.
func wireRuntime(d *pluginDeps) {
	logf := d.logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	store, err := pluginrt.OpenStore(d.stateDir, d.libDir)
	if err != nil {
		logf("plugins: runtime disabled: %v", err)
		return
	}
	currentPluginDeps = d
	var mgr *pluginrt.Manager
	sup := newPluginSupervisor(d, func(id, reason string) {
		if mgr != nil {
			mgr.MarkFailed(id, reason)
		}
	})
	allow := func() bool { return d.getCfg == nil || d.getCfg().PluginsAllowUnverified() }
	mgr = pluginrt.NewManager(pluginrt.Deps{
		Store:           store,
		Registry:        func() plugin.Registry { return d.registry },
		Host:            func() plugin.HostRegistry { return d.hostReg },
		Supervisor:      sup,
		Fetch:           pluginFetch,
		AllowUnverified: allow,
		CoreVersion:     d.coreVersion,
		GOOS:            runtime.GOOS,
		GOARCH:          runtime.GOARCH,
		Now:             d.now,
		Validate:        plugin.ValidateValues,
		Coerce:          plugin.CoerceStrings,
		WithDefaults:    plugin.WithDefaults,
		Redact:          plugin.Redact,
		Logf:            logf,
	})
	d.lifecycle = mgr
	d.inventory = mgr.Inventory()
	if d.registry != nil {
		d.starts = append(d.starts, func() { d.registry.OnRevoke(mgr.ApplyRevocations) })
	}
	d.starts = append(d.starts, mgr.StartEnabled)
	d.stops = append(d.stops, mgr.StopAll)
}

func pluginFetch(ctx context.Context, url string) (io.ReadCloser, error) {
	client := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to %s", req.URL.Scheme)
			}
			if len(via) > 5 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}
