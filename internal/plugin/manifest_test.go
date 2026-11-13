package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validManifest() Manifest {
	return Manifest{
		Schema: 1, ID: "demo", Name: "Demo", Version: "1.0.0", Publisher: "Acme", Description: "d",
		HostAPI: "^1.0", Permissions: []Permission{PermMetricsRead},
		Binaries: []Binary{{OS: "linux", Arch: "amd64", SHA256: hex64, Size: 10}},
		ConfigSchema: &ConfigSchema{Type: "object", Properties: map[string]*SchemaProp{
			"url": {Type: "string", Title: "URL"},
		}},
		Contributes: Contributes{
			Slots:   []SlotDecl{{Slot: SlotNavPage, ID: "main", Title: "Main"}},
			Actions: []ActionDecl{{ID: "go", Label: "Go", MinRole: RoleAdmin}},
			Files:   []FileDecl{{Name: "x.json", Title: "X", ContentType: "application/json"}},
		},
	}
}

func TestManifestValidate(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		name, field string
		mut         func(m *Manifest)
	}{
		{"schema", "schema", func(m *Manifest) { m.Schema = 2 }},
		{"id", "id", func(m *Manifest) { m.ID = "Bad_ID" }},
		{"name empty", "name", func(m *Manifest) { m.Name = "" }},
		{"name long", "name", func(m *Manifest) { m.Name = strings.Repeat("x", 65) }},
		{"version", "version", func(m *Manifest) { m.Version = "nope" }},
		{"publisher", "publisher", func(m *Manifest) { m.Publisher = "" }},
		{"description", "description", func(m *Manifest) { m.Description = strings.Repeat("x", 281) }},
		{"homepage", "homepage", func(m *Manifest) { m.Homepage = "http://x" }},
		{"host_api", "host_api", func(m *Manifest) { m.HostAPI = "x" }},
		{"min_trinetra", "min_trinetra", func(m *Manifest) { m.MinTrinetra = "zzz" }},
		{"unknown permission", "permissions", func(m *Manifest) { m.Permissions = []Permission{"bogus"} }},
		{"duplicate permission", "permissions", func(m *Manifest) { m.Permissions = []Permission{PermMetricsRead, PermMetricsRead} }},
		{"no binaries", "binaries", func(m *Manifest) { m.Binaries = nil }},
		{"binary os", "binaries", func(m *Manifest) { m.Binaries[0].OS = "windows" }},
		{"binary arch", "binaries", func(m *Manifest) { m.Binaries[0].Arch = "mips" }},
		{"binary sha", "binaries", func(m *Manifest) { m.Binaries[0].SHA256 = "ABC" }},
		{"binary size", "binaries", func(m *Manifest) { m.Binaries[0].Size = 0 }},
		{"binary dup", "binaries", func(m *Manifest) { m.Binaries = append(m.Binaries, m.Binaries[0]) }},
		{"activation", "activation", func(m *Manifest) { m.Activation = "sometimes" }},
		{"slot unknown", "slot", func(m *Manifest) { m.Contributes.Slots[0].Slot = "x.y" }},
		{"slot id", "slot", func(m *Manifest) { m.Contributes.Slots[0].ID = "Bad" }},
		{"slot dup", "slot", func(m *Manifest) { m.Contributes.Slots = append(m.Contributes.Slots, m.Contributes.Slots[0]) }},
		{"slot title", "slot", func(m *Manifest) { m.Contributes.Slots[0].Title = "" }},
		{"slot role", "slot", func(m *Manifest) { m.Contributes.Slots[0].MinRole = "root" }},
		{"settings.form needs schema", "slot", func(m *Manifest) {
			m.ConfigSchema = nil
			m.Contributes.Slots = []SlotDecl{{Slot: SlotSettingsForm, ID: "s", Title: "S"}}
		}},
		{"alert.rules needs section props", "slot", func(m *Manifest) {
			m.Contributes.Slots = []SlotDecl{{Slot: SlotAlertRules, ID: "r", Title: "R"}}
		}},
		{"container.action needs action", "slot", func(m *Manifest) {
			m.Contributes.Slots = []SlotDecl{{Slot: SlotContainerAction, ID: "missing", Title: "T"}}
		}},
		{"action id", "action", func(m *Manifest) { m.Contributes.Actions[0].ID = "Bad" }},
		{"action dup", "action", func(m *Manifest) { m.Contributes.Actions = append(m.Contributes.Actions, m.Contributes.Actions[0]) }},
		{"action label", "action", func(m *Manifest) { m.Contributes.Actions[0].Label = "" }},
		{"action role", "action", func(m *Manifest) { m.Contributes.Actions[0].MinRole = "" }},
		{"action permission", "action", func(m *Manifest) { m.Contributes.Actions[0].Permission = "bogus" }},
		{"file name", "file", func(m *Manifest) { m.Contributes.Files[0].Name = "../x" }},
		{"file dup", "file", func(m *Manifest) { m.Contributes.Files = append(m.Contributes.Files, m.Contributes.Files[0]) }},
		{"file type", "file", func(m *Manifest) { m.Contributes.Files[0].ContentType = "text/html" }},
		{"config_schema", "config_schema", func(m *Manifest) { m.ConfigSchema.Type = "array" }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			m := validManifest()
			r.mut(&m)
			err := m.Validate()
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), r.field) {
				t.Fatalf("error %q lacks %q", err, r.field)
			}
		})
	}
}

func TestManifestAcceptsContainerActionWithAction(t *testing.T) {
	m := validManifest()
	m.Contributes.Slots = []SlotDecl{{Slot: SlotContainerAction, ID: "go", Title: "Go"}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestManifestRejectsReservedPermission(t *testing.T) {
	m := validManifest()
	m.Permissions = []Permission{PermExec}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "reserved for a later release") {
		t.Fatalf("got %v", err)
	}
}

func TestDecodeManifestRejectsUnknownField(t *testing.T) {
	b, _ := json.Marshal(validManifest())
	if _, err := DecodeManifest(b); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(b), `{"schema"`, `{"extra":1,"schema"`, 1)
	if _, err := DecodeManifest([]byte(bad)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDecodeManifestRejectsTrailingBytes(t *testing.T) {
	b, _ := json.Marshal(validManifest())
	if _, err := DecodeManifest(append(b, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing bytes accepted")
	}
}

func TestDecodeManifestRejectsOversize(t *testing.T) {
	if _, err := DecodeManifest(make([]byte, MaxManifestBytes+1)); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestManifestLookups(t *testing.T) {
	m := validManifest()
	if b, ok := m.BinaryFor("linux", "amd64"); !ok || b.Size != 10 {
		t.Fatal("BinaryFor")
	}
	if _, ok := m.BinaryFor("linux", "arm"); ok {
		t.Fatal("BinaryFor arm")
	}
	if _, ok := m.SlotDecl(SlotNavPage, "main"); !ok {
		t.Fatal("SlotDecl")
	}
	if _, ok := m.SlotDecl(SlotNavPage, "other"); ok {
		t.Fatal("SlotDecl other")
	}
	if _, ok := m.ActionDecl("go"); !ok {
		t.Fatal("ActionDecl")
	}
	if BinaryName("demo") != "trinetra-plugin-demo" || BinaryPath("linux", "arm64", "demo") != "bin/linux-arm64/trinetra-plugin-demo" {
		t.Fatal("binary names")
	}
	if RoleRank(RoleViewer) != 1 || RoleRank(RoleResponder) != 2 || RoleRank(RoleAdmin) != 3 || RoleRank("x") != 0 {
		t.Fatal("RoleRank")
	}
}
