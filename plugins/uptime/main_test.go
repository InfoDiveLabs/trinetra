package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/registry/regtool"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk/pluginsdktest"
)

type jsonRaw = json.RawMessage

func raw1(s string) jsonRaw { return jsonRaw(s) }

var perms = []plugin.Permission{plugin.PermNetOutbound, plugin.PermMetricsPublish, plugin.PermAlertsRaise}

func TestManifestValid(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.Abs(".")
	if err := regtool.Check(dir); err != nil {
		t.Fatalf("regtool.Check: %v", err)
	}
	loose := strings.Replace(string(raw), `"binaries": []`,
		`"binaries": [{"os":"linux","arch":"amd64","sha256":"`+strings.Repeat("a", 64)+`","size":1}]`, 1)
	m, err := plugin.DecodeManifest([]byte(loose))
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if m.ID != "uptime" {
		t.Fatalf("id = %q", m.ID)
	}
	want := map[plugin.Permission]bool{plugin.PermNetOutbound: true, plugin.PermMetricsPublish: true, plugin.PermAlertsRaise: true}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions = %v", m.Permissions)
	}
	for _, p := range m.Permissions {
		if !want[p] {
			t.Fatalf("unexpected permission %q", p)
		}
	}
	for _, s := range []struct {
		slot plugin.Slot
		id   string
	}{{plugin.SlotSettingsForm, "settings"}, {plugin.SlotAlertRules, "rules"}, {plugin.SlotNavPage, "checks"}, {plugin.SlotDashboardPanel, "overview"}} {
		if _, ok := m.SlotDecl(s.slot, s.id); !ok {
			t.Errorf("slot %s/%s missing", s.slot, s.id)
		}
	}
	if errs := plugin.ValidateValues(m.ConfigSchema, map[string]jsonRaw{
		"checks": raw1(`["http https://example.com"]`), "default_interval": raw1(`"30s"`), "down_after_runs": raw1(`3`),
	}); len(errs) != 0 {
		t.Fatalf("sample config invalid: %v", errs)
	}
	if errs := plugin.ValidateValues(m.ConfigSchema, map[string]jsonRaw{"default_interval": raw1(`"1s"`)}); len(errs) == 0 {
		t.Fatal("interval below 10s should be rejected by the schema")
	}
}

func TestValidateConfigReportsLineErrors(t *testing.T) {
	h := pluginsdktest.New(t, "uptime", perms, map[string]any{"checks": []string{}})
	a := newApp(&Prober{})
	h.Start(a.p)
	cfg := pluginsdk.Config{Values: map[string]jsonRaw{"checks": raw1(`["http https://ok.test name=a", "ping x", "http https://ok.test name=a", "tcp nohost"]`)}}
	errs := validateConfig(cfg)
	msg := errs["checks"]
	for _, want := range []string{"line 2 col 1", "line 3: name \"a\" already used on line 1", "line 4 col 5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	ok := pluginsdk.Config{Values: map[string]jsonRaw{"checks": raw1(`["tcp a:1", "", "dns example.com"]`)}}
	if errs := validateConfig(ok); len(errs) != 0 {
		t.Fatalf("valid config reported %v", errs)
	}
}

func TestViewsRender(t *testing.T) {
	h := pluginsdktest.New(t, "uptime", perms, map[string]any{"checks": []string{"tcp a.test:1 name=alpha", "tls b.test name=beta"}})
	a := newApp(&Prober{})
	a.sched.jitter = func(time.Duration) time.Duration { return time.Hour }
	h.Start(a.p)
	t.Cleanup(a.sched.Stop)
	waitFor(t, "config applied", func() bool { cs, _ := a.store.Snapshot(); return len(cs) == 2 })

	res, err := h.Render("checks", plugin.SlotContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Widgets) != 2 || res.Widgets[0].Kind != plugin.WidgetStatus || res.Widgets[1].Kind != plugin.WidgetTable {
		t.Fatalf("checks widgets = %+v", res.Widgets)
	}
	for _, it := range res.Widgets[0].Status.Items {
		if it.State != "unknown" {
			t.Errorf("%s state = %s before any run", it.Name, it.State)
		}
	}
	if len(res.Widgets[1].Table.Rows) != 2 || len(res.Widgets[1].Table.Rows[0]) != len(res.Widgets[1].Table.Columns) {
		t.Fatalf("table = %+v", res.Widgets[1].Table)
	}

	a.store.Record(Check{Name: "alpha"}, Result{OK: true, Latency: 12 * time.Millisecond, At: time.Now()})
	a.store.Record(Check{Name: "beta"}, Result{HasCert: true, CertDays: 5, Detail: "expired soon", At: time.Now()})
	res, _ = h.Render("checks", plugin.SlotContext{})
	items := res.Widgets[0].Status.Items
	if items[0].State != "up" || items[0].Detail != "12 ms" || items[1].State != "down" || items[1].Detail != "expired soon" {
		t.Fatalf("items = %+v", items)
	}
	if got := res.Widgets[1].Table.Rows[1][5]; got != "5" {
		t.Errorf("cert days cell = %q", got)
	}

	ov, err := h.Render("overview", plugin.SlotContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Widgets) != 2 || ov.Widgets[0].Kind != plugin.WidgetTimeseries || ov.Widgets[1].Kind != plugin.WidgetStat {
		t.Fatalf("overview = %+v", ov.Widgets)
	}
	ts := ov.Widgets[0].Timeseries
	if len(ts.Series) != 2 || ts.Series[0].Metric != "plugin.uptime.check.alpha.latency_ms" {
		t.Fatalf("series = %+v", ts.Series)
	}
	if st := ov.Widgets[1].Stat; st.Value != "1" || st.Tone != "warn" {
		t.Fatalf("stat = %+v", st)
	}
}

func TestOverviewCapsSeriesAndEmptyHint(t *testing.T) {
	var cs []Check
	for i := 0; i < 20; i++ {
		cs = append(cs, Check{Name: "c" + string(rune('a'+i))})
	}
	ov := Overview(cs, nil)
	if n := len(ov[0].Timeseries.Series); n != plugin.MaxSeriesPerChart {
		t.Fatalf("series = %d, want %d", n, plugin.MaxSeriesPerChart)
	}
	if ov[1].Stat.Value != "0" || ov[1].Stat.Tone != "ok" {
		t.Fatalf("stat = %+v", ov[1].Stat)
	}
	if w := ChecksPage(nil, nil); len(w) != 1 || w[0].Kind != plugin.WidgetText {
		t.Fatalf("empty page = %+v", w)
	}
}

func TestAgainstFakeHost(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer web.Close()
	cert, pool := certFor(t, time.Now().Add(3*24*time.Hour))
	addr := tlsServer(t, cert)

	h := pluginsdktest.New(t, "uptime", perms, map[string]any{
		"checks":           []string{"http " + web.URL + " name=web", "tls " + addr + " name=cert"},
		"default_interval": "30ms",
		"default_timeout":  "2s",
		"down_after_runs":  2,
		"cert_warn_days":   14,
	})
	a := newApp(&Prober{RootCAs: pool})
	a.sched.jitter = func(time.Duration) time.Duration { return 0 }
	h.Start(a.p)
	t.Cleanup(a.sched.Stop)

	waitFor(t, "series published", func() bool {
		var up, lat, days bool
		for _, p := range h.Published() {
			_, u := p.Metrics["check.web.up"]
			_, l := p.Metrics["check.web.latency_ms"]
			_, d := p.Metrics["cert.cert.days_left"]
			up, lat, days = up || u, lat || l, days || d
		}
		return up && lat && days
	})
	waitFor(t, "cert alert", func() bool {
		for _, al := range h.Alerts() {
			if al.Key == "cert.cert.expiring" && al.Severity == "warning" {
				return true
			}
		}
		return false
	})
	if h.Status().State != "ok" {
		t.Fatalf("status = %+v", h.Status())
	}
	for _, al := range h.Alerts() {
		if al.Key == "check.web.down" {
			t.Fatalf("healthy check raised %+v", al)
		}
	}

	healthy.Store(false)
	waitFor(t, "down alert", func() bool {
		for _, al := range h.Alerts() {
			if al.Key == "check.web.down" && al.Severity == "critical" {
				return true
			}
		}
		return false
	})
	healthy.Store(true)
	waitFor(t, "down resolved", func() bool {
		for _, r := range h.Resolved() {
			if r.Key == "check.web.down" {
				return true
			}
		}
		return false
	})

	res, err := h.Render("checks", plugin.SlotContext{})
	if err != nil || len(res.Widgets) != 2 {
		t.Fatalf("render: %v %+v", err, res)
	}

	h.SetConfig(map[string]any{"checks": []string{"tcp " + strings.TrimPrefix(web.URL, "http://") + " name=web2"}, "default_interval": "30ms"})
	waitFor(t, "removed check alerts resolved", func() bool {
		for _, r := range h.Resolved() {
			if r.Key == "cert.cert.expiring" {
				return true
			}
		}
		return false
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
