package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// var _ = func(a API) API { return a } asserts, at compile time, that API is referenceable
// as a type: this file fails to compile until api.go defines the interface.
var _ = func(a API) API { return a }

func TestAlertRecordJSONKeys(t *testing.T) {
	b, _ := json.Marshal(AlertRecord{Key: "cpu", Severity: "warning"})
	if !strings.Contains(string(b), `"key"`) || !strings.Contains(string(b), `"severity"`) {
		t.Fatalf("unexpected json: %s", b)
	}
}
