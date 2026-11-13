package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const errUnknownSetting = "unknown setting"

// ValidateValues checks values against s; nil when valid.
func ValidateValues(s *ConfigSchema, values map[string]json.RawMessage) FieldErrors {
	errs := FieldErrors{}
	if s == nil {
		s = &ConfigSchema{}
	}
	for k := range values {
		if _, ok := s.Properties[k]; !ok {
			errs[k] = errUnknownSetting
		}
	}
	full := WithDefaults(s, values)
	for _, k := range s.Required {
		if v, ok := full[k]; !ok || isNull(v) {
			errs[k] = "required"
		}
	}
	for k, v := range values {
		p := s.Properties[k]
		if p == nil {
			continue
		}
		if msg := validateValue(p, v); msg != "" {
			errs[k] = msg
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func isNull(v json.RawMessage) bool { return string(bytes.TrimSpace(v)) == "null" }

func validateValue(p *SchemaProp, v json.RawMessage) string {
	if isNull(v) {
		return "must not be null"
	}
	switch p.Type {
	case "string":
		var s string
		if json.Unmarshal(v, &s) != nil {
			return "must be a string"
		}
		return validateString(p, s)
	case "integer":
		var f float64
		if json.Unmarshal(v, &f) != nil {
			return "must be a whole number"
		}
		if f != math.Trunc(f) || math.IsInf(f, 0) || math.Abs(f) > 1<<53 {
			return "must be a whole number"
		}
		return checkRange(p, f, "")
	case "number":
		var f float64
		if json.Unmarshal(v, &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return "must be a finite number"
		}
		return checkRange(p, f, "")
	case "boolean":
		var b bool
		if json.Unmarshal(v, &b) != nil {
			return "must be true or false"
		}
		return ""
	case "array":
		var items []json.RawMessage
		if json.Unmarshal(v, &items) != nil {
			return "must be a list of strings"
		}
		limit := MaxArrayItems
		if p.MaxItems > 0 {
			limit = p.MaxItems
		}
		if len(items) > limit {
			return fmt.Sprintf("at most %d items", limit)
		}
		for i, it := range items {
			var s string
			if json.Unmarshal(it, &s) != nil {
				return fmt.Sprintf("item %d must be a string", i+1)
			}
			ip := p.Items
			if ip == nil {
				ip = &SchemaProp{Type: "string"}
			}
			if msg := validateString(ip, s); msg != "" {
				return fmt.Sprintf("item %d: %s", i+1, msg)
			}
		}
		return ""
	}
	return "unsupported type"
}

func validateString(p *SchemaProp, s string) string {
	if len(s) > MaxStringValue {
		return fmt.Sprintf("at most %d bytes", MaxStringValue)
	}
	if p.Format == FormatDuration {
		d, err := time.ParseDuration(s)
		if err != nil {
			return "must be a duration such as 30s or 5m"
		}
		return checkRange(p, d.Seconds(), "s")
	}
	if len(p.Enum) > 0 {
		for _, e := range p.Enum {
			if e == s {
				return ""
			}
		}
		return "must be one of " + strings.Join(p.Enum, ", ")
	}
	if p.Pattern != "" {
		re, err := regexp.Compile("^(?:" + p.Pattern + ")$")
		if err != nil || !re.MatchString(s) {
			return "does not match the required pattern"
		}
	}
	return ""
}

func checkRange(p *SchemaProp, f float64, unit string) string {
	if p.Minimum != nil && f < *p.Minimum {
		return fmt.Sprintf("must be at least %v%s", *p.Minimum, unit)
	}
	if p.Maximum != nil && f > *p.Maximum {
		return fmt.Sprintf("must be at most %v%s", *p.Maximum, unit)
	}
	return ""
}

// CoerceStrings converts string-form values to typed JSON per s.
func CoerceStrings(s *ConfigSchema, kv map[string]string) (map[string]json.RawMessage, FieldErrors) {
	out := map[string]json.RawMessage{}
	errs := FieldErrors{}
	if s == nil {
		s = &ConfigSchema{}
	}
	for k, v := range kv {
		p := s.Properties[k]
		if p == nil {
			errs[k] = errUnknownSetting
			continue
		}
		raw, msg := coerceOne(p, v)
		if msg != "" {
			errs[k] = msg
			continue
		}
		out[k] = raw
	}
	if len(errs) == 0 {
		errs = nil
	}
	return out, errs
}

func coerceOne(p *SchemaProp, v string) (json.RawMessage, string) {
	switch p.Type {
	case "string":
		b, _ := json.Marshal(v)
		return b, ""
	case "integer":
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil, "must be a whole number"
		}
		return json.RawMessage(strconv.FormatInt(n, 10)), ""
	case "number":
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, "must be a finite number"
		}
		return json.RawMessage(strconv.FormatFloat(f, 'g', -1, 64)), ""
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "on", "1":
			return json.RawMessage("true"), ""
		case "false", "no", "off", "0":
			return json.RawMessage("false"), ""
		}
		return nil, "must be true or false"
	case "array":
		items := []string{}
		for _, line := range strings.Split(v, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				items = append(items, line)
			}
		}
		b, _ := json.Marshal(items)
		return b, ""
	}
	return nil, "unsupported type"
}

// WithDefaults returns values with schema defaults filled for missing keys.
func WithDefaults(s *ConfigSchema, values map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(values))
	for k, v := range values {
		out[k] = v
	}
	if s == nil {
		return out
	}
	for k, p := range s.Properties {
		if _, ok := out[k]; !ok && p != nil && len(p.Default) > 0 {
			out[k] = p.Default
		}
	}
	return out
}

// Redact removes secret values and lists which secrets are set.
func Redact(s *ConfigSchema, values map[string]json.RawMessage) (map[string]json.RawMessage, []string) {
	out := make(map[string]json.RawMessage, len(values))
	var set []string
	for k, v := range values {
		if s != nil {
			if p := s.Properties[k]; p != nil && p.Format == FormatSecret {
				set = append(set, k)
				continue
			}
		}
		out[k] = v
	}
	sort.Strings(set)
	return out, set
}
