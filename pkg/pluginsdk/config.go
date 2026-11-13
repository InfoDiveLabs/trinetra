package pluginsdk

import (
	"encoding/json"
	"time"
)

// Config is a plugin's settings at one revision, with schema defaults already applied by the host.
type Config struct {
	Revision int64
	Values   map[string]json.RawMessage
}

// String returns the string setting key, or "" when unset or not a string.
func (c Config) String(key string) string {
	var s string
	if json.Unmarshal(c.Values[key], &s) != nil {
		return ""
	}
	return s
}

// Int returns the integer setting key, or 0.
func (c Config) Int(key string) int64 {
	var f float64
	if json.Unmarshal(c.Values[key], &f) != nil {
		return 0
	}
	return int64(f)
}

// Float returns the numeric setting key, or 0.
func (c Config) Float(key string) float64 {
	var f float64
	if json.Unmarshal(c.Values[key], &f) != nil {
		return 0
	}
	return f
}

// Bool returns the boolean setting key, or false.
func (c Config) Bool(key string) bool {
	var b bool
	if json.Unmarshal(c.Values[key], &b) != nil {
		return false
	}
	return b
}

// Duration returns the duration setting key (a string such as "30s"), or 0.
func (c Config) Duration(key string) time.Duration {
	d, err := time.ParseDuration(c.String(key))
	if err != nil {
		return 0
	}
	return d
}

// Strings returns the list-of-strings setting key, or nil.
func (c Config) Strings(key string) []string {
	var out []string
	if json.Unmarshal(c.Values[key], &out) != nil {
		return nil
	}
	return out
}
