package trinetra

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// newTelegramTestMaster is newTestMasterState (fleet_provider_test.go) plus a real silence
// store, so CreateSilence (used by the "sil1h:" callback) works.
func newTelegramTestMaster(t *testing.T) *masterState {
	t.Helper()
	m := newTestMasterState(t)
	silences, err := loadSilenceStore(filepath.Join(t.TempDir(), "silences.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.silences = silences
	return m
}

// openIncidentFor registers a node and fires one alert on it, returning the
// resulting incident's id.
func openIncidentFor(t *testing.T, m *masterState, nodeName, key string) (nodeID, incidentID string) {
	t.Helper()
	id, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: id, Name: nodeName, Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	m.engine.Submit(alertSource{NodeID: id, NodeName: nodeName}, Alert{Key: key, Kind: "fire", Severity: SevWarning, Time: 1000})
	incs, err := fleetAPIFor(m).Incidents(core.IncidentFilter{})
	if err != nil || len(incs) != 1 {
		t.Fatalf("incidents = %+v err %v", incs, err)
	}
	return id, incs[0].ID
}

func cbUpdate(chat, data string) telegram.Update {
	return telegram.Update{CallbackID: "cbq1", CallbackData: data, CallbackChat: chat}
}

// TestTelegramCallbackAckAuthorized: an "ack:<id>" callback from the
// enrolled owner chat acknowledges the incident and answers "acked".
func TestTelegramCallbackAckAuthorized(t *testing.T) {
	m := newTelegramTestMaster(t)
	_, incID := openIncidentFor(t, m, "web1", "cpu")
	api := fleetAPIFor(m)

	text := telegramCallbackAnswer(api, "999", cbUpdate("999", "ack:"+incID), time.Now)
	if text != "acked" {
		t.Fatalf("answer = %q, want acked", text)
	}
	inc, err := api.Incident(incID)
	if err != nil {
		t.Fatal(err)
	}
	if inc.AckedBy != "telegram" {
		t.Fatalf("AckedBy = %q, want telegram", inc.AckedBy)
	}
}

// TestTelegramCallbackSilence1hAuthorized: a "sil1h:<id>" callback from the enrolled owner
// chat creates a 1h silence matching the incident's members.
func TestTelegramCallbackSilence1hAuthorized(t *testing.T) {
	m := newTelegramTestMaster(t)
	nodeID, incID := openIncidentFor(t, m, "web1", "cpu")
	api := fleetAPIFor(m)
	now := time.Unix(1_700_000_000, 0)

	text := telegramCallbackAnswer(api, "999", cbUpdate("999", "sil1h:"+incID), func() time.Time { return now })
	if text != "silenced 1h" {
		t.Fatalf("answer = %q, want \"silenced 1h\"", text)
	}
	sils, err := api.Silences()
	if err != nil || len(sils) != 1 {
		t.Fatalf("silences = %+v err %v", sils, err)
	}
	s := sils[0]
	if s.Author != "telegram" || s.Comment != "from Telegram" {
		t.Fatalf("silence = %+v", s)
	}
	if s.Start != now.Unix() || s.End != now.Add(time.Hour).Unix() {
		t.Fatalf("silence window = [%d, %d), want [%d, %d)", s.Start, s.End, now.Unix(), now.Add(time.Hour).Unix())
	}
	if len(s.Matchers) != 1 || s.Matchers[0].Node != nodeID || s.Matchers[0].Rule != "cpu" {
		t.Fatalf("matchers = %+v", s.Matchers)
	}
}

// TestTelegramCallbackSilence1hOnResolvedIncidentCreatesNoSilence: a "sil1h:<id>" callback
// on an already-resolved incident.
func TestTelegramCallbackSilence1hOnResolvedIncidentCreatesNoSilence(t *testing.T) {
	m := newTelegramTestMaster(t)
	nodeID, incID := openIncidentFor(t, m, "web1", "cpu")
	m.engine.Submit(alertSource{NodeID: nodeID, NodeName: "web1"}, Alert{Key: "cpu", Kind: "recover", Severity: SevWarning, Time: 1050})
	api := fleetAPIFor(m)

	inc, err := api.Incident(incID)
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != "resolved" {
		t.Fatalf("test setup: incident state = %q, want resolved before exercising the callback", inc.State)
	}

	text := telegramCallbackAnswer(api, "999", cbUpdate("999", "sil1h:"+incID), time.Now)
	if text != "unknown/expired" {
		t.Fatalf("answer = %q, want \"unknown/expired\"", text)
	}
	sils, err := api.Silences()
	if err != nil {
		t.Fatal(err)
	}
	if len(sils) != 0 {
		t.Fatalf("a resolved incident's sil1h callback created a silence: %+v", sils)
	}
}

// TestTelegramCallbackForeignChatRejected: a callback from any chat other than the enrolled
// owner is answered "not authorized" and performs no action.
func TestTelegramCallbackForeignChatRejected(t *testing.T) {
	m := newTelegramTestMaster(t)
	_, incID := openIncidentFor(t, m, "web1", "cpu")
	api := fleetAPIFor(m)

	text := telegramCallbackAnswer(api, "999", cbUpdate("111", "ack:"+incID), time.Now)
	if text != "not authorized" {
		t.Fatalf("answer = %q, want \"not authorized\"", text)
	}
	inc, err := api.Incident(incID)
	if err != nil {
		t.Fatal(err)
	}
	if inc.AckedBy != "" {
		t.Fatalf("incident was acked despite a foreign-chat callback: %+v", inc)
	}
}

// TestTelegramCallbackUnclaimedBotRejected: before any owner chat is enrolled at all
// (ownerChatID == ""), every callback is "not authorized".
func TestTelegramCallbackUnclaimedBotRejected(t *testing.T) {
	m := newTelegramTestMaster(t)
	_, incID := openIncidentFor(t, m, "web1", "cpu")
	api := fleetAPIFor(m)

	text := telegramCallbackAnswer(api, "", cbUpdate("999", "ack:"+incID), time.Now)
	if text != "not authorized" {
		t.Fatalf("answer = %q, want \"not authorized\"", text)
	}
}

// TestTelegramCallbackUnknownDataAnswersUnknownExpired covers data that
// doesn't match either known prefix.
func TestTelegramCallbackUnknownDataAnswersUnknownExpired(t *testing.T) {
	m := newTelegramTestMaster(t)
	api := fleetAPIFor(m)
	text := telegramCallbackAnswer(api, "999", cbUpdate("999", "snooze:abc"), time.Now)
	if text != "unknown/expired" {
		t.Fatalf("answer = %q, want \"unknown/expired\"", text)
	}
}

// TestTelegramCallbackUnknownIncidentAnswersUnknownExpired covers an ack:/sil1h: id that no
// longer resolves to an incident.
func TestTelegramCallbackUnknownIncidentAnswersUnknownExpired(t *testing.T) {
	m := newTelegramTestMaster(t)
	api := fleetAPIFor(m)
	for _, data := range []string{"ack:deadbeefdead", "sil1h:deadbeefdead"} {
		if text := telegramCallbackAnswer(api, "999", cbUpdate("999", data), time.Now); text != "unknown/expired" {
			t.Fatalf("data %q: answer = %q, want \"unknown/expired\"", data, text)
		}
	}
}

// TestTelegramCallbackSoloUnchanged: solo/child's own core.FleetAPI fails every write with
// core.ErrNotMaster (requireMaster).
func TestTelegramCallbackSoloUnchanged(t *testing.T) {
	p := &fleetProvider{role: config.RoleSolo, self: fleetCLIFakeAPI{}, selfName: func() string { return "solo1" }}
	api := p.Fleet()
	for _, data := range []string{"ack:abcdef012345", "sil1h:abcdef012345"} {
		if text := telegramCallbackAnswer(api, "999", cbUpdate("999", data), time.Now); text != "unknown/expired" {
			t.Fatalf("data %q on solo: answer = %q, want \"unknown/expired\"", data, text)
		}
	}
}

// TestTelegramCallbackNilFleetAPIAnswersUnknownExpired is a defensive belt-and-braces
// check: telegramCallbackAnswer must not panic on a nil fleetAPI.
func TestTelegramCallbackNilFleetAPIAnswersUnknownExpired(t *testing.T) {
	text := telegramCallbackAnswer(nil, "999", cbUpdate("999", "ack:abcdef012345"), time.Now)
	if text != "unknown/expired" {
		t.Fatalf("answer = %q, want \"unknown/expired\"", text)
	}
}
