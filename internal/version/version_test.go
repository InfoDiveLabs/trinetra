package version

import "testing"

func TestStringFallbackNeverEmpty(t *testing.T) {
	// With no build-time stamp (the default in a plain `go test` binary),
	// String() must still return a non-empty, sensible value, never "".
	orig := Version
	defer func() { Version = orig }()

	Version = ""
	if got := String(); got == "" {
		t.Error("String() with empty Version returned \"\", want a fallback")
	}

	Version = "v1.2.3"
	if got := String(); got != "v1.2.3" {
		t.Errorf("String() = %q, want the stamped v1.2.3", got)
	}
}
