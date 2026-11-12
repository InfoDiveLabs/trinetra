package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestPluginOfferJSONRoundTrip(t *testing.T) {
	in := PluginOffer{
		ID: "uptime", Version: "1.2.0", SHA256: strings.Repeat("ab", 32), Size: 1234,
		Trust:   plugin.TrustVerified,
		Consent: plugin.ConsentRecord{Actor: "root", Via: "cli", Version: "1.2.0", Nodes: []string{"n1"}},
		Config:  map[string]string{"interval": "30"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out PluginOffer
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.SHA256 != in.SHA256 || out.Size != in.Size || out.Trust != in.Trust ||
		out.Consent.Via != "cli" || len(out.Consent.Nodes) != 1 || out.Config["interval"] != "30" {
		t.Fatalf("round trip = %+v", out)
	}
}

func TestLiveUpdateWithoutPluginsUnchanged(t *testing.T) {
	b, err := json.Marshal(LiveUpdate{Version: "v1", Snapshot: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "plugins") {
		t.Fatalf("empty Plugins serialised: %s", b)
	}
	var old LiveUpdate
	if err := json.Unmarshal([]byte(`{"sent_at":1,"version":"v1","snapshot":{},"outbox":{}}`), &old); err != nil || old.Plugins != nil {
		t.Fatalf("old json: %v %+v", err, old)
	}
	b, _ = json.Marshal(LiveUpdate{Plugins: []PluginReport{{ID: "x", Phase: "installed"}}})
	if !strings.Contains(string(b), `"plugins":[{"id":"x","phase":"installed"}]`) {
		t.Fatalf("plugins missing: %s", b)
	}
}

func TestChildIgnoresUnknownFrameTypes(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	frames := make(chan Frame, 4)
	_, id, _ := startShipperWithFrames(t, f, func(fr Frame) { frames <- fr }, false)
	waitFor(t, "connected", func() bool { return hub.Connected(id) })
	offer, _ := json.Marshal(PluginOffer{ID: "x"})
	hub.Push(id, Frame{Type: FramePluginOffer, Data: offer})
	hub.Push(id, Frame{Type: "lease", Data: json.RawMessage(`{"until":1}`)})
	var got []string
	deadline := time.After(2 * time.Second)
	for len(got) < 2 {
		select {
		case fr := <-frames:
			got = append(got, fr.Type)
		case <-deadline:
			t.Fatalf("frames after plugin_offer lost: %v", got)
		}
	}
	if got[0] != FramePluginOffer || got[1] != "lease" {
		t.Fatalf("got %v", got)
	}
}
