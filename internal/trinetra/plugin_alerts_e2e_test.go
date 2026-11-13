package trinetra

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/pluginhost"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
)

func TestPluginKillMidIncidentCoreAlertsStillDeliver(t *testing.T) {
	root := hostapiShortDir(t)
	stateDir := filepath.Join(root, "state")
	var mu sync.Mutex
	var sent []string
	disp := NewDispatcher([]Channel{{
		N: recordingNotifier{name: "rec", sent: &sent, mu: &mu}, Enabled: true, Route: Route{MinSeverity: SevInfo},
	}}, dispatcherTimeout)
	q := NewNotifierQueue(disp, 16)

	d := &pluginDeps{
		stateDir:     stateDir,
		hostDir:      filepath.Join(root, "run"),
		inventory:    newHostTestInventory(enabledInstalled("probe", plugin.PermAlertsRaise)),
		deliverAlert: func(a Alert) { q.Enqueue(a, false) },
		submitSeries: func(int64, MetricSet) bool { return true },
		now:          time.Now,
	}
	wireHost(d)
	for _, s := range d.starts {
		s()
	}
	defer func() {
		for _, s := range d.stops {
			s()
		}
	}()
	svc := d.hostReg.(*pluginhost.Service)
	tok, err := svc.Register("probe", []plugin.Permission{plugin.PermAlertsRaise}, false)
	if err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(root, "token")
	if err := os.WriteFile(tokFile, []byte(tok), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		plugin.EnvPluginID: "probe", plugin.EnvHostSocket: svc.HostSocket(),
		plugin.EnvPluginSocket: filepath.Join(root, "p.sock"), plugin.EnvTokenFile: tokFile,
	}
	p := pluginsdk.New(pluginsdk.Options{})
	p.SetEnv(func(k string) string { return env[k] })
	ctx, kill := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for p.Hello().PluginID == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.Hello().PluginID != "probe" {
		t.Fatal("plugin never connected through the host socket")
	}
	if err := p.RaiseAlert("check.down", "critical", "Endpoint down", "no response"); err != nil {
		t.Fatal(err)
	}

	kill()
	if err := <-done; err != nil {
		t.Fatalf("plugin exit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "p.sock")); err == nil {
		t.Fatal("plugin socket still present after kill")
	}

	q.Enqueue(Alert{Key: "cpu", Title: "CPU high", Severity: SevCritical, Kind: "fire", Source: "core", Time: time.Now().Unix()}, false)
	qctx, qcancel := context.WithCancel(context.Background())
	defer qcancel()
	go q.Run(qctx)

	want := map[string]bool{"plugin.probe.check.down": false, "cpu": false}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, k := range sent {
			if _, ok := want[k]; ok {
				want[k] = true
			}
		}
		all := want["plugin.probe.check.down"] && want["cpu"]
		mu.Unlock()
		if all {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !want["plugin.probe.check.down"] || !want["cpu"] {
		t.Fatalf("delivered %v, want both plugin and core alerts", sent)
	}

	act := svc.Alerts.Active()
	if len(act) != 1 || act[0].Key != "plugin.probe.check.down" || act[0].Severity != "critical" {
		t.Fatalf("active after kill: %+v", act)
	}
	reopened, err := pluginhost.OpenAlertBook(filepath.Join(stateDir, "plugins", "alerts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if act := reopened.Active(); len(act) != 1 || act[0].Key != "plugin.probe.check.down" {
		t.Fatalf("active after reopen: %+v", act)
	}
}
