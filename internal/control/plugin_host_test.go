package control

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakeHost struct {
	mu      sync.Mutex
	logs    []plugin.LogEntry
	ids     []string
	configs []plugin.ConfigResponse
}

func (h *fakeHost) Hello(id string, r plugin.HelloRequest) (plugin.HelloResponse, error) {
	return plugin.HelloResponse{PluginID: id, HostAPI: r.HostAPI}, nil
}
func (h *fakeHost) GetConfig(id string) (plugin.ConfigResponse, error) {
	return plugin.ConfigResponse{Revision: 1}, nil
}
func (h *fakeHost) WatchConfig(ctx context.Context, id string) (<-chan plugin.ConfigResponse, error) {
	ch := make(chan plugin.ConfigResponse, len(h.configs))
	for _, c := range h.configs {
		ch <- c
	}
	close(ch)
	return ch, nil
}
func (h *fakeHost) ReportStatus(id string, r plugin.StatusReport) error { return nil }
func (h *fakeHost) Log(id string, e plugin.LogEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids = append(h.ids, id)
	h.logs = append(h.logs, e)
	return nil
}
func (h *fakeHost) PublishSeries(id string, r plugin.PublishRequest) (plugin.PublishResponse, error) {
	return plugin.PublishResponse{Accepted: len(r.Metrics)}, nil
}
func (h *fakeHost) RaiseAlert(id string, r plugin.AlertRequest) error     { return nil }
func (h *fakeHost) ResolveAlert(id string, r plugin.ResolveRequest) error { return nil }
func (h *fakeHost) Snapshot(id string) (core.DashboardView, error)        { return core.DashboardView{}, nil }
func (h *fakeHost) Series(id string, r plugin.SeriesRequest) ([]core.SeriesPoint, error) {
	return nil, nil
}
func (h *fakeHost) ActiveAlerts(id string) ([]core.AlertRecord, error) { return nil, nil }
func (h *fakeHost) Containers(id string) ([]core.ContainerView, error) { return nil, nil }
func (h *fakeHost) ContainerLogs(id string, r plugin.ContainerLogsRequest) (string, error) {
	return "", nil
}
func (h *fakeHost) FleetNodes(id string) ([]core.NodeSummary, error) { return nil, nil }

var _ plugin.Host = (*fakeHost)(nil)

func TestPluginHostRoutesWithAuthenticatedID(t *testing.T) {
	h := &fakeHost{}
	tk := NewTokens()
	tk.Set("tok", Grant{PluginID: "alpha", Methods: plugin.AllowedMethods(nil)})
	env, ok := dialAuth(t, &fakeAPI{}, h, tk, "tok")
	if !ok {
		t.Fatal("rejected")
	}
	r := env.call(t, 1, plugin.MethodLog, plugin.LogEntry{Level: "info", Message: "hi"}, "")
	if !r.OK || len(h.ids) != 1 || h.ids[0] != "alpha" || h.logs[0].Message != "hi" {
		t.Fatalf("resp=%+v ids=%v logs=%v", r, h.ids, h.logs)
	}
}

func TestPluginHostUnavailableWithoutHost(t *testing.T) {
	tk := NewTokens()
	tk.Set("tok", Grant{PluginID: "alpha", Methods: plugin.AllowedMethods(nil)})
	env, _ := dialAuth(t, &fakeAPI{}, nil, tk, "tok")
	r := env.call(t, 1, plugin.MethodGetConfig, struct{}{}, "")
	if r.OK || r.Error != plugin.ErrUnavailable.Error() {
		t.Fatalf("got %+v", r)
	}
}

func TestWatchConfigStreams(t *testing.T) {
	h := &fakeHost{configs: []plugin.ConfigResponse{{Revision: 2}, {Revision: 3}}}
	tk := NewTokens()
	tk.Set("tok", Grant{PluginID: "alpha", Methods: plugin.AllowedMethods(nil)})
	env, _ := dialAuth(t, &fakeAPI{}, h, tk, "tok")
	if r := env.call(t, 9, plugin.MethodWatchConfig, struct{}{}, ""); !r.OK || r.ID != 9 {
		t.Fatalf("ack %+v", r)
	}
	for _, want := range []int64{2, 3} {
		var f response
		if err := readFrame(env.r, &f); err != nil {
			t.Fatal(err)
		}
		var c plugin.ConfigResponse
		if f.ID != plugin.StreamID || json.Unmarshal(f.Result, &c) != nil || c.Revision != want {
			t.Fatalf("frame %+v cfg %+v", f, c)
		}
	}
}
