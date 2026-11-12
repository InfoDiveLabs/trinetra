package trinetra

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func newTestPluginChild(t *testing.T, lc *fleetFakeLifecycle, allowUnverified bool) *pluginChild {
	t.Helper()
	return newPluginChild(func() plugin.Lifecycle { return lc }, func() plugin.Registry { return nil },
		func() bool { return allowUnverified }, "master-1", t.TempDir(), nil)
}

func offerFrame(t *testing.T, data []byte, trust plugin.Trust, cfg map[string]string) (fleet.Frame, chunkFetcher) {
	t.Helper()
	o := fleet.PluginOffer{
		ID: "x", Version: "1.0.0", SHA256: shaOf(data), Size: int64(len(data)), Trust: trust,
		Consent: plugin.ConsentRecord{Actor: "root", Via: "cli", Version: "1.0.0", Permissions: []plugin.Permission{"metrics.write"}, Unverified: trust == plugin.TrustUnverified, Nodes: []string{"a"}},
		Config:  cfg,
	}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var offs []int64
	return fleet.Frame{Type: fleet.FramePluginOffer, Data: b}, sliceFetcher(data, 64, &offs)
}

func TestChildVerifiedOfferRequiresOwnSignatureCheck(t *testing.T) {
	lc := newFleetFakeLifecycle()
	c := newTestPluginChild(t, lc, false)
	data := []byte("verified package bytes")
	f, fetch := offerFrame(t, data, plugin.TrustVerified, nil)
	c.HandleFrame(f, fetch)
	c.wg.Wait()
	if len(lc.installs) != 1 {
		t.Fatalf("installs = %d", len(lc.installs))
	}
	in := lc.installs[0]
	if !in.RequireVerified || in.ExpectSHA256 != shaOf(data) || in.Consent == nil || in.Actor != "fleet" {
		t.Fatalf("install request = %+v", in)
	}
	if r := c.Reports(); len(r) != 1 || r[0].Phase != phaseEnabled {
		t.Fatalf("reports = %+v", r)
	}
}

func TestChildUnverifiedRejectedWhenDisallowed(t *testing.T) {
	lc := newFleetFakeLifecycle()
	c := newTestPluginChild(t, lc, false)
	f, fetch := offerFrame(t, []byte("unverified bytes"), plugin.TrustUnverified, nil)
	c.HandleFrame(f, fetch)
	c.wg.Wait()
	if len(lc.installs) != 0 {
		t.Fatal("unverified offer installed while disallowed")
	}
	r := c.Reports()
	if len(r) != 1 || r[0].Phase != phaseFailed || r[0].Error != "unverified plugins are disabled on this node" {
		t.Fatalf("reports = %+v", r)
	}
}

func TestChildUnverifiedAcceptedWhenAllowed(t *testing.T) {
	lc := newFleetFakeLifecycle()
	c := newTestPluginChild(t, lc, true)
	f, fetch := offerFrame(t, []byte("unverified bytes"), plugin.TrustUnverified, nil)
	c.HandleFrame(f, fetch)
	c.wg.Wait()
	if len(lc.installs) != 1 || lc.installs[0].RequireVerified {
		t.Fatalf("installs = %+v", lc.installs)
	}
}

func TestChildRecordsMasterConsentVia(t *testing.T) {
	lc := newFleetFakeLifecycle()
	lc.enabled = false
	c := newTestPluginChild(t, lc, true)
	f, fetch := offerFrame(t, []byte("pkg"), plugin.TrustVerified, map[string]string{"interval": "30"})
	c.HandleFrame(f, fetch)
	c.wg.Wait()
	in := lc.installs[0]
	if in.Via != "fleet:master-1" || in.Consent.Via != "fleet:master-1" || in.Consent.Actor != "root" {
		t.Fatalf("via/consent = %q %+v", in.Via, in.Consent)
	}
	if len(lc.configs) != 1 || lc.configs[0].Values["interval"] != "30" || lc.configs[0].Via != "fleet:master-1" {
		t.Fatalf("configs = %+v", lc.configs)
	}
	if len(lc.enables) != 1 || lc.enables[0].Via != "fleet:master-1" {
		t.Fatalf("enables = %+v", lc.enables)
	}
}

func TestChildRemoveAndRevoke(t *testing.T) {
	lc := newFleetFakeLifecycle()
	lc.installed["x"] = plugin.Status{Installed: plugin.Installed{ID: "x", Version: "1.0.0", State: plugin.StateEnabled}}
	lc.installed["y"] = plugin.Status{Installed: plugin.Installed{ID: "y", Version: "2.0.0", State: plugin.StateEnabled}}
	c := newTestPluginChild(t, lc, false)

	rm, _ := json.Marshal(fleet.PluginRemove{ID: "x"})
	c.HandleFrame(fleet.Frame{Type: fleet.FramePluginRemove, Data: rm}, nil)
	rv, _ := json.Marshal(fleet.PluginRevoke{Revocations: []plugin.Revocation{
		{ID: "y", Versions: []string{"1.0.0"}, Reason: "other version"},
		{ID: "y", Versions: []string{"2.0.0"}, Reason: "bad"},
	}})
	c.HandleFrame(fleet.Frame{Type: fleet.FramePluginRevoke, Data: rv}, nil)
	c.wg.Wait()
	if len(lc.removes) != 1 || lc.removes[0] != "x/fleet/fleet:master-1" {
		t.Fatalf("removes = %v", lc.removes)
	}
	if len(lc.disables) != 1 || lc.disables[0] != "y" {
		t.Fatalf("disables = %v", lc.disables)
	}
}

func TestChildReportsPhases(t *testing.T) {
	lc := newFleetFakeLifecycle()
	lc.installErr = errors.New("bad signature")
	c := newTestPluginChild(t, lc, false)
	f, fetch := offerFrame(t, []byte("pkg"), plugin.TrustVerified, nil)
	c.HandleFrame(f, fetch)
	c.wg.Wait()
	r := c.Reports()
	if len(r) != 1 || r[0].Phase != phaseFailed || r[0].Error != "bad signature" || r[0].ID != "x" {
		t.Fatalf("reports = %+v", r)
	}
	lc.installErr = nil
	c.HandleFrame(f, fetch)
	c.wg.Wait()
	if r := c.Reports(); r[0].Phase != phaseEnabled || r[0].Received != r[0].Size {
		t.Fatalf("reports = %+v", r)
	}
}

func TestChildSkipsOfferAlreadyInstalled(t *testing.T) {
	lc := newFleetFakeLifecycle()
	data := []byte("pkg")
	lc.installed["x"] = plugin.Status{Installed: plugin.Installed{ID: "x", PackageSHA256: shaOf(data), State: plugin.StateEnabled}}
	c := newTestPluginChild(t, lc, false)
	f, fetch := offerFrame(t, data, plugin.TrustVerified, nil)
	c.HandleFrame(f, fetch)
	c.wg.Wait()
	if len(lc.installs) != 0 {
		t.Fatal("re-offered installed package was reinstalled")
	}
	if r := c.Reports(); r[0].Phase != phaseEnabled {
		t.Fatalf("reports = %+v", r)
	}
}
