package pluginhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func openBook(t *testing.T, path string, out *[]AlertOut) *AlertBook {
	t.Helper()
	b, err := OpenAlertBook(path, func(a AlertOut) { *out = append(*out, a) }, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func raiseReq(key, sev string) plugin.AlertRequest {
	return plugin.AlertRequest{Key: key, Severity: sev, Title: "down", Body: "b"}
}

func TestRaiseDeliversFireOnceAndPersistsBeforeReturn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts.json")
	var seen bool
	b, _ := OpenAlertBook(path, func(a AlertOut) {
		raw, err := os.ReadFile(path)
		seen = err == nil && strings.Contains(string(raw), "plugin.probe.check.down")
		if a.Kind != "fire" || a.Source != "plugin:probe" || a.Key != "plugin.probe.check.down" || a.Severity != "critical" || a.Time != fixedNow.Unix() {
			t.Errorf("alert %+v", a)
		}
	}, func() time.Time { return fixedNow })
	if err := b.Raise("probe", raiseReq("check.down", "critical")); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("not persisted before deliver")
	}
}

func TestRaiseIdempotent(t *testing.T) {
	var out []AlertOut
	b := openBook(t, filepath.Join(t.TempDir(), "a.json"), &out)
	_ = b.Raise("probe", raiseReq("k", "warning"))
	_ = b.Raise("probe", raiseReq("k", "warning"))
	if len(out) != 1 {
		t.Fatalf("delivered %d", len(out))
	}
	_ = b.Raise("probe", raiseReq("k", "critical"))
	if len(out) != 2 || out[1].Severity != "critical" || out[1].Kind != "fire" {
		t.Fatalf("severity change: %+v", out)
	}
}

func TestResolveDeliversRecover(t *testing.T) {
	var out []AlertOut
	b := openBook(t, filepath.Join(t.TempDir(), "a.json"), &out)
	_ = b.Raise("probe", raiseReq("k", "warning"))
	if err := b.Resolve("probe", plugin.ResolveRequest{Key: "k", Body: "ok now"}); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Kind != "recover" || out[1].Body != "ok now" || out[1].Title != "down" {
		t.Fatalf("%+v", out)
	}
	if len(b.Active()) != 0 {
		t.Fatal("still active")
	}
}

func TestResolveUnknownIsNoop(t *testing.T) {
	var out []AlertOut
	b := openBook(t, filepath.Join(t.TempDir(), "a.json"), &out)
	if err := b.Resolve("probe", plugin.ResolveRequest{Key: "nope"}); err != nil || len(out) != 0 {
		t.Fatalf("err=%v out=%v", err, out)
	}
	_ = b.Raise("probe", raiseReq("k", "info"))
	_ = b.Resolve("other", plugin.ResolveRequest{Key: "k"})
	if len(b.Active()) != 1 {
		t.Fatal("other plugin resolved it")
	}
}

func TestResolveAllOnDisable(t *testing.T) {
	var out []AlertOut
	b := openBook(t, filepath.Join(t.TempDir(), "a.json"), &out)
	_ = b.Raise("probe", raiseReq("a", "info"))
	_ = b.Raise("probe", raiseReq("b", "info"))
	_ = b.Raise("other", raiseReq("a", "info"))
	b.ResolveAll("probe", "plugin disabled")
	act := b.Active()
	if len(act) != 1 || act[0].Key != "plugin.other.a" {
		t.Fatalf("%+v", act)
	}
	recov := 0
	for _, a := range out {
		if a.Kind == "recover" && a.Body == "plugin disabled" {
			recov++
		}
	}
	if recov != 2 {
		t.Fatalf("recover=%d", recov)
	}
}

func TestAlertBookReloadKeepsActive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	var out []AlertOut
	b := openBook(t, path, &out)
	_ = b.Raise("probe", raiseReq("k", "critical"))
	_ = b.Ack("plugin.probe.k", 42)
	out = nil
	b2 := openBook(t, path, &out)
	act := b2.Active()
	if len(act) != 1 || act[0].Key != "plugin.probe.k" || act[0].Severity != "critical" || !act[0].Acked || act[0].AckedAt != 42 || act[0].Source != "plugin:probe" || act[0].Title != "down" {
		t.Fatalf("%+v", act)
	}
	_ = b2.Raise("probe", raiseReq("k", "critical"))
	if len(out) != 0 {
		t.Fatal("re-raise after reload re-fired")
	}
}

func TestAlertCapPerPlugin(t *testing.T) {
	var out []AlertOut
	b := openBook(t, filepath.Join(t.TempDir(), "a.json"), &out)
	for i := 0; i < plugin.MaxActiveAlertsPerPlugin; i++ {
		if err := b.Raise("probe", raiseReq(fmt.Sprintf("k%d", i), "info")); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Raise("probe", raiseReq("overflow", "info")); err == nil {
		t.Fatal("cap not enforced")
	}
	if err := b.Raise("other", raiseReq("k0", "info")); err != nil {
		t.Fatal("cap leaked across plugins")
	}
	if err := b.Raise("probe", raiseReq("k0", "critical")); err != nil {
		t.Fatal("existing key update refused")
	}
}

func TestAlertKeyValidation(t *testing.T) {
	var out []AlertOut
	b := openBook(t, filepath.Join(t.TempDir(), "a.json"), &out)
	bad := []plugin.AlertRequest{
		{Key: "", Severity: "info", Title: "t"},
		{Key: "Upper", Severity: "info", Title: "t"},
		{Key: "a/b", Severity: "info", Title: "t"},
		{Key: strings.Repeat("a", 97), Severity: "info", Title: "t"},
		{Key: "k", Severity: "fatal", Title: "t"},
		{Key: "k", Severity: "info", Title: ""},
		{Key: "k", Severity: "info", Title: strings.Repeat("x", 201)},
		{Key: "k", Severity: "info", Title: "t", Body: strings.Repeat("x", 4097)},
	}
	for i, r := range bad {
		if err := b.Raise("probe", r); err == nil {
			t.Errorf("row %d accepted", i)
		}
	}
	if len(out) != 0 || len(b.Active()) != 0 {
		t.Fatal("bad alerts leaked")
	}
	if !b.Owns("plugin.x.y") || b.Owns("cpu") {
		t.Fatal("Owns")
	}
}

func TestAckUnack(t *testing.T) {
	var out []AlertOut
	b := openBook(t, filepath.Join(t.TempDir(), "a.json"), &out)
	_ = b.Raise("probe", raiseReq("k", "info"))
	if err := b.Ack("plugin.probe.k", 7); err != nil {
		t.Fatal(err)
	}
	if a := b.Active()[0]; !a.Acked || a.AckedAt != 7 {
		t.Fatalf("%+v", a)
	}
	if err := b.Unack("plugin.probe.k"); err != nil {
		t.Fatal(err)
	}
	if a := b.Active()[0]; a.Acked {
		t.Fatal("still acked")
	}
	if b.Ack("plugin.probe.zzz", 1) == nil || b.Unack("plugin.probe.zzz") == nil {
		t.Fatal("unknown key accepted")
	}
}
