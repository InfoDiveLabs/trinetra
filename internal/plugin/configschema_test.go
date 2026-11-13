package plugin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func vSchema() *ConfigSchema {
	return &ConfigSchema{Type: "object", Required: []string{"url"}, Properties: map[string]*SchemaProp{
		"url":     {Type: "string", Title: "URL", Pattern: "https?://.+"},
		"mode":    {Type: "string", Title: "Mode", Enum: []string{"a", "b"}},
		"token":   {Type: "string", Format: FormatSecret, Title: "Token"},
		"every":   {Type: "string", Format: FormatDuration, Title: "Every", Minimum: f64(5), Maximum: f64(3600)},
		"count":   {Type: "integer", Title: "Count", Minimum: f64(1), Maximum: f64(10), Default: raw("3")},
		"ratio":   {Type: "number", Title: "Ratio"},
		"enabled": {Type: "boolean", Title: "On"},
		"hosts":   {Type: "array", Title: "Hosts", MaxItems: 2, Items: &SchemaProp{Type: "string", Pattern: "[a-z]+"}},
	}}
}

func TestValidateValuesTable(t *testing.T) {
	good := map[string]json.RawMessage{"url": raw(`"http://x"`)}
	if errs := ValidateValues(vSchema(), good); errs != nil {
		t.Fatalf("good: %v", errs)
	}
	u := raw(`"http://x"`)
	rows := []struct {
		name string
		vals map[string]json.RawMessage
		key  string
	}{
		{"missing required", map[string]json.RawMessage{}, "url"},
		{"pattern fail", map[string]json.RawMessage{"url": raw(`"ftp://x"`)}, "url"},
		{"string too long", map[string]json.RawMessage{"url": raw(`"http://` + strings.Repeat("a", MaxStringValue) + `"`)}, "url"},
		{"not a string", map[string]json.RawMessage{"url": raw(`5`)}, "url"},
		{"enum fail", map[string]json.RawMessage{"url": u, "mode": raw(`"c"`)}, "mode"},
		{"enum ok", map[string]json.RawMessage{"url": u, "mode": raw(`"a"`)}, ""},
		{"secret string ok", map[string]json.RawMessage{"url": u, "token": raw(`"s"`)}, ""},
		{"secret not string", map[string]json.RawMessage{"url": u, "token": raw(`1`)}, "token"},
		{"duration ok", map[string]json.RawMessage{"url": u, "every": raw(`"1m"`)}, ""},
		{"duration bad", map[string]json.RawMessage{"url": u, "every": raw(`"soon"`)}, "every"},
		{"duration below min", map[string]json.RawMessage{"url": u, "every": raw(`"1s"`)}, "every"},
		{"duration above max", map[string]json.RawMessage{"url": u, "every": raw(`"2h"`)}, "every"},
		{"integer ok", map[string]json.RawMessage{"url": u, "count": raw(`5`)}, ""},
		{"integer fraction", map[string]json.RawMessage{"url": u, "count": raw(`2.5`)}, "count"},
		{"integer above max", map[string]json.RawMessage{"url": u, "count": raw(`11`)}, "count"},
		{"integer string", map[string]json.RawMessage{"url": u, "count": raw(`"3"`)}, "count"},
		{"number ok", map[string]json.RawMessage{"url": u, "ratio": raw(`1.5`)}, ""},
		{"number string", map[string]json.RawMessage{"url": u, "ratio": raw(`"x"`)}, "ratio"},
		{"boolean ok", map[string]json.RawMessage{"url": u, "enabled": raw(`true`)}, ""},
		{"boolean bad", map[string]json.RawMessage{"url": u, "enabled": raw(`"yes"`)}, "enabled"},
		{"array ok", map[string]json.RawMessage{"url": u, "hosts": raw(`["a","b"]`)}, ""},
		{"array too many", map[string]json.RawMessage{"url": u, "hosts": raw(`["a","b","c"]`)}, "hosts"},
		{"array item pattern", map[string]json.RawMessage{"url": u, "hosts": raw(`["a1"]`)}, "hosts"},
		{"array not strings", map[string]json.RawMessage{"url": u, "hosts": raw(`[1]`)}, "hosts"},
		{"unknown key", map[string]json.RawMessage{"url": u, "zzz": raw(`1`)}, "zzz"},
		{"null value", map[string]json.RawMessage{"url": raw(`null`)}, "url"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			errs := ValidateValues(vSchema(), r.vals)
			if r.key == "" {
				if errs != nil {
					t.Fatalf("want ok, got %v", errs)
				}
				return
			}
			if errs[r.key] == "" {
				t.Fatalf("want error on %q, got %v", r.key, errs)
			}
		})
	}
	if e := ValidateValues(vSchema(), map[string]json.RawMessage{"zzz": raw(`1`)})["zzz"]; e != "unknown setting" {
		t.Fatalf("unknown msg %q", e)
	}
}

func TestValidateRequiredSatisfiedByDefault(t *testing.T) {
	s := &ConfigSchema{Type: "object", Required: []string{"n"}, Properties: map[string]*SchemaProp{
		"n": {Type: "integer", Title: "n", Default: raw("2")},
	}}
	if errs := ValidateValues(s, nil); errs != nil {
		t.Fatal(errs)
	}
}

func TestPatternIsAnchored(t *testing.T) {
	s := &ConfigSchema{Type: "object", Properties: map[string]*SchemaProp{"a": {Type: "string", Title: "a", Pattern: "[a-z]+"}}}
	if errs := ValidateValues(s, map[string]json.RawMessage{"a": raw(`"abc1"`)}); errs["a"] == "" {
		t.Fatal("abc1 should be rejected")
	}
	if errs := ValidateValues(s, map[string]json.RawMessage{"a": raw(`"abc"`)}); errs != nil {
		t.Fatal(errs)
	}
	s.Properties["a"].Pattern = "a|b"
	if errs := ValidateValues(s, map[string]json.RawMessage{"a": raw(`"ab"`)}); errs["a"] == "" {
		t.Fatal("alternation must be anchored as a whole")
	}
}

func TestCoerceStrings(t *testing.T) {
	out, errs := CoerceStrings(vSchema(), map[string]string{
		"url": "http://x", "count": "7", "ratio": "0.5", "enabled": "Yes", "hosts": " a \n\n b\n", "every": "30s", "token": "t",
	})
	if errs != nil {
		t.Fatal(errs)
	}
	want := map[string]string{"url": `"http://x"`, "count": `7`, "ratio": `0.5`, "enabled": `true`, "hosts": `["a","b"]`, "every": `"30s"`, "token": `"t"`}
	for k, w := range want {
		if string(out[k]) != w {
			t.Errorf("%s = %s want %s", k, out[k], w)
		}
	}
	for _, v := range []string{"true", "yes", "on", "1"} {
		o, _ := CoerceStrings(vSchema(), map[string]string{"enabled": v})
		if string(o["enabled"]) != "true" {
			t.Errorf("%s should be true", v)
		}
	}
	for _, v := range []string{"false", "no", "off", "0"} {
		o, _ := CoerceStrings(vSchema(), map[string]string{"enabled": v})
		if string(o["enabled"]) != "false" {
			t.Errorf("%s should be false", v)
		}
	}
	_, errs = CoerceStrings(vSchema(), map[string]string{"count": "x", "enabled": "maybe", "ratio": "NaN", "nope": "1"})
	for _, k := range []string{"count", "enabled", "ratio", "nope"} {
		if errs[k] == "" {
			t.Errorf("want error on %s: %v", k, errs)
		}
	}
	if errs["nope"] != "unknown setting" {
		t.Errorf("nope = %q", errs["nope"])
	}
}

func TestWithDefaults(t *testing.T) {
	in := map[string]json.RawMessage{"url": raw(`"u"`)}
	out := WithDefaults(vSchema(), in)
	if string(out["count"]) != "3" || string(out["url"]) != `"u"` {
		t.Fatalf("%v", out)
	}
	if _, ok := in["count"]; ok {
		t.Fatal("input mutated")
	}
	in["count"] = raw("9")
	if string(WithDefaults(vSchema(), in)["count"]) != "9" {
		t.Fatal("explicit value overridden")
	}
	if WithDefaults(nil, nil) == nil {
		t.Fatal("want non-nil map")
	}
}

func TestRedactDropsSecrets(t *testing.T) {
	s := vSchema()
	s.Properties["token2"] = &SchemaProp{Type: "string", Format: FormatSecret, Title: "t2"}
	vals := map[string]json.RawMessage{"url": raw(`"u"`), "token": raw(`"s"`), "token2": raw(`"s2"`)}
	out, names := Redact(s, vals)
	if _, ok := out["token"]; ok {
		t.Fatal("secret kept")
	}
	if string(out["url"]) != `"u"` {
		t.Fatal("url lost")
	}
	if !reflect.DeepEqual(names, []string{"token", "token2"}) {
		t.Fatalf("names %v", names)
	}
	if _, ok := vals["token"]; !ok {
		t.Fatal("input mutated")
	}
}
