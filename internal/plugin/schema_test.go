package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func f64(v float64) *float64 { return &v }

func okSchema() *ConfigSchema {
	return &ConfigSchema{Type: "object", Properties: map[string]*SchemaProp{
		"url": {Type: "string", Title: "URL"},
	}}
}

func TestSchemaValidate(t *testing.T) {
	if err := okSchema().Validate(); err != nil {
		t.Fatal(err)
	}
	many := map[string]*SchemaProp{}
	for i := 0; i < 65; i++ {
		many["k"+strings.Repeat("a", i%10)+string(rune('a'+i%26))+string(rune('a'+i/26))] = &SchemaProp{Type: "string", Title: "x"}
	}
	rows := []struct {
		name string
		mut  func(s *ConfigSchema)
	}{
		{"type not object", func(s *ConfigSchema) { s.Type = "array" }},
		{"too many properties", func(s *ConfigSchema) { s.Properties = many }},
		{"bad key name", func(s *ConfigSchema) { s.Properties["Bad-Key"] = &SchemaProp{Type: "string", Title: "x"} }},
		{"bad prop type", func(s *ConfigSchema) { s.Properties["url"].Type = "object" }},
		{"format on integer", func(s *ConfigSchema) {
			s.Properties["url"] = &SchemaProp{Type: "integer", Format: FormatSecret, Title: "x"}
		}},
		{"unknown format", func(s *ConfigSchema) { s.Properties["url"].Format = "uri" }},
		{"enum on integer", func(s *ConfigSchema) {
			s.Properties["url"] = &SchemaProp{Type: "integer", Enum: []string{"a"}, Title: "x"}
		}},
		{"enum with format", func(s *ConfigSchema) {
			s.Properties["url"].Enum = []string{"a"}
			s.Properties["url"].Format = FormatSecret
		}},
		{"empty enum entry list too long", func(s *ConfigSchema) {
			e := make([]string, 65)
			for i := range e {
				e[i] = "v" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			}
			s.Properties["url"].Enum = e
		}},
		{"bad pattern", func(s *ConfigSchema) { s.Properties["url"].Pattern = "(" }},
		{"pattern on integer", func(s *ConfigSchema) { s.Properties["url"] = &SchemaProp{Type: "integer", Pattern: "a", Title: "x"} }},
		{"minimum on string", func(s *ConfigSchema) { s.Properties["url"].Minimum = f64(1) }},
		{"minimum above maximum", func(s *ConfigSchema) {
			s.Properties["url"] = &SchemaProp{Type: "integer", Minimum: f64(5), Maximum: f64(1), Title: "x"}
		}},
		{"array without items", func(s *ConfigSchema) { s.Properties["url"] = &SchemaProp{Type: "array", Title: "x"} }},
		{"array items not string", func(s *ConfigSchema) {
			s.Properties["url"] = &SchemaProp{Type: "array", Items: &SchemaProp{Type: "integer"}, Title: "x"}
		}},
		{"array items with format", func(s *ConfigSchema) {
			s.Properties["url"] = &SchemaProp{Type: "array", Items: &SchemaProp{Type: "string", Format: FormatSecret}, Title: "x"}
		}},
		{"maxItems too large", func(s *ConfigSchema) {
			s.Properties["url"] = &SchemaProp{Type: "array", Items: &SchemaProp{Type: "string"}, MaxItems: MaxArrayItems + 1, Title: "x"}
		}},
		{"bad section", func(s *ConfigSchema) { s.Properties["url"].Section = SlotNavPage }},
		{"required missing", func(s *ConfigSchema) { s.Required = []string{"nope"} }},
		{"order missing", func(s *ConfigSchema) { s.Order = []string{"nope"} }},
		{"default kind mismatch", func(s *ConfigSchema) { s.Properties["url"].Default = json.RawMessage(`5`) }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := okSchema()
			r.mut(s)
			if err := s.Validate(); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestSchemaDefaultsAndSections(t *testing.T) {
	s := okSchema()
	s.Properties["n"] = &SchemaProp{Type: "integer", Title: "N", Default: json.RawMessage(`3`), Section: SlotAlertRules}
	s.Properties["l"] = &SchemaProp{Type: "array", Title: "L", Items: &SchemaProp{Type: "string"}, Default: json.RawMessage(`["a"]`)}
	s.Properties["b"] = &SchemaProp{Type: "boolean", Title: "B", Default: json.RawMessage(`true`)}
	s.Order = []string{"n"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Properties["url"].SectionOf() != SlotSettingsForm || s.Properties["n"].SectionOf() != SlotAlertRules {
		t.Fatal("SectionOf wrong")
	}
	got := strings.Join(s.OrderedKeys(), ",")
	if got != "n,b,l,url" {
		t.Fatalf("OrderedKeys = %s", got)
	}
}
