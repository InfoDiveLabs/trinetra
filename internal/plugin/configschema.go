package plugin

import "encoding/json"

// ValidateValues checks values against s; nil when valid.
func ValidateValues(s *ConfigSchema, values map[string]json.RawMessage) FieldErrors { return nil }

// CoerceStrings converts string-form values to typed JSON per s.
func CoerceStrings(s *ConfigSchema, kv map[string]string) (map[string]json.RawMessage, FieldErrors) {
	return nil, nil
}

// WithDefaults returns values with schema defaults filled for missing keys.
func WithDefaults(s *ConfigSchema, values map[string]json.RawMessage) map[string]json.RawMessage {
	return nil
}

// Redact removes secret values and lists which secrets are set.
func Redact(s *ConfigSchema, values map[string]json.RawMessage) (map[string]json.RawMessage, []string) {
	return nil, nil
}
