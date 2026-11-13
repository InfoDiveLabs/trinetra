package plugin

import "testing"

func TestParseHostAPI(t *testing.T) {
	ok := map[string][2]int{"^1.0": {1, 0}, "1.2": {1, 2}}
	for in, want := range ok {
		maj, min, err := ParseHostAPI(in)
		if err != nil || maj != want[0] || min != want[1] {
			t.Fatalf("%q -> %d %d %v", in, maj, min, err)
		}
	}
	for _, in := range []string{"", "1", "^x.1", ">=1.0"} {
		if _, _, err := ParseHostAPI(in); err == nil {
			t.Fatalf("%q should fail", in)
		}
	}
}

func TestHostAPICompatible(t *testing.T) {
	if err := HostAPICompatible("^1.0"); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"^1.1", "^2.0", "^0.9", "bogus"} {
		if err := HostAPICompatible(in); err == nil {
			t.Fatalf("%q should be incompatible", in)
		}
	}
	if HostAPIString() != "1.0" {
		t.Fatal(HostAPIString())
	}
}
