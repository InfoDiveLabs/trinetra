package plugin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// ConfigSchema is the JSON-Schema subset for plugin settings.
type ConfigSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]*SchemaProp `json:"properties"`
	Required   []string               `json:"required,omitempty"`
	Order      []string               `json:"x-order,omitempty"`
}

// SchemaProp is one setting.
type SchemaProp struct {
	Type        string          `json:"type"`
	Format      string          `json:"format,omitempty"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Section     Slot            `json:"x-section,omitempty"`
	Enum        []string        `json:"enum,omitempty"`
	Pattern     string          `json:"pattern,omitempty"`
	Minimum     *float64        `json:"minimum,omitempty"`
	Maximum     *float64        `json:"maximum,omitempty"`
	MaxItems    int             `json:"maxItems,omitempty"`
	Items       *SchemaProp     `json:"items,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
}

const (
	FormatSecret   = "secret"
	FormatDuration = "duration"
	MaxStringValue = 4096
	MaxArrayItems  = 200
)

const maxSchemaProps = 64

// SectionOf returns p.Section, defaulting to SlotSettingsForm.
func (p *SchemaProp) SectionOf() Slot {
	if p.Section == "" {
		return SlotSettingsForm
	}
	return p.Section
}

// OrderedKeys returns Order first, then remaining property names sorted.
func (s *ConfigSchema) OrderedKeys() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range s.Order {
		if _, ok := s.Properties[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	var rest []string
	for k := range s.Properties {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// Validate checks the schema's structure.
func (s *ConfigSchema) Validate() error {
	if s.Type != "object" {
		return fmt.Errorf("config_schema: type must be \"object\"")
	}
	if len(s.Properties) > maxSchemaProps {
		return fmt.Errorf("config_schema: at most %d properties", maxSchemaProps)
	}
	for _, k := range s.OrderedKeys() {
		if !KeyPattern.MatchString(k) {
			return fmt.Errorf("config_schema: property name %q is invalid", k)
		}
		if err := validateProp(k, s.Properties[k]); err != nil {
			return fmt.Errorf("config_schema: %w", err)
		}
	}
	for _, k := range s.Required {
		if _, ok := s.Properties[k]; !ok {
			return fmt.Errorf("config_schema: required %q is not a property", k)
		}
	}
	for _, k := range s.Order {
		if _, ok := s.Properties[k]; !ok {
			return fmt.Errorf("config_schema: x-order %q is not a property", k)
		}
	}
	return nil
}

func validateProp(name string, p *SchemaProp) error {
	if p == nil {
		return fmt.Errorf("%s: missing definition", name)
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%s: "+format, append([]any{name}, a...)...)
	}
	switch p.Type {
	case "string", "integer", "number", "boolean", "array":
	default:
		return bad("type %q is not supported", p.Type)
	}
	switch p.Format {
	case "", FormatSecret, FormatDuration:
	default:
		return bad("format %q is not supported", p.Format)
	}
	if p.Format != "" && p.Type != "string" {
		return bad("format applies only to string")
	}
	if len(p.Enum) > 0 {
		if p.Type != "string" {
			return bad("enum applies only to string")
		}
		if p.Format != "" {
			return bad("enum cannot be combined with a format")
		}
		if len(p.Enum) > 64 {
			return bad("enum has more than 64 entries")
		}
	}
	if p.Pattern != "" {
		if p.Type != "string" {
			return bad("pattern applies only to string")
		}
		if _, err := regexp.Compile(p.Pattern); err != nil {
			return bad("pattern: %v", err)
		}
	}
	if p.Minimum != nil || p.Maximum != nil {
		numeric := p.Type == "integer" || p.Type == "number" || p.Format == FormatDuration
		if !numeric {
			return bad("minimum/maximum apply only to numbers and durations")
		}
		if p.Minimum != nil && p.Maximum != nil && *p.Minimum > *p.Maximum {
			return bad("minimum exceeds maximum")
		}
	}
	if p.MaxItems < 0 || p.MaxItems > MaxArrayItems {
		return bad("maxItems must be between 0 and %d", MaxArrayItems)
	}
	if p.Type == "array" {
		if p.Items == nil || p.Items.Type != "string" || p.Items.Format != "" {
			return bad("array items must be plain strings")
		}
	}
	switch p.Section {
	case "", SlotSettingsForm, SlotAlertRules:
	default:
		return bad("x-section %q is not allowed", p.Section)
	}
	if len(p.Default) > 0 {
		var v any
		if err := json.Unmarshal(p.Default, &v); err != nil {
			return bad("default is not JSON")
		}
		ok := false
		switch p.Type {
		case "string":
			_, ok = v.(string)
		case "integer":
			f, isNum := v.(float64)
			ok = isNum && f == float64(int64(f))
		case "number":
			_, ok = v.(float64)
		case "boolean":
			_, ok = v.(bool)
		case "array":
			_, ok = v.([]any)
		}
		if !ok {
			return bad("default does not match type %s", p.Type)
		}
	}
	return nil
}
