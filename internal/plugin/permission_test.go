package plugin

import (
	"reflect"
	"testing"
)

func TestDescribeCoversEveryV1Permission(t *testing.T) {
	seen := map[string]Permission{}
	for _, p := range V1Permissions {
		d := Describe(p)
		if d == "" || d == "unknown permission" {
			t.Fatalf("%s has no description", p)
		}
		if prev, dup := seen[d]; dup {
			t.Fatalf("%s and %s share a description", p, prev)
		}
		seen[d] = p
	}
	if Describe("nope") != "unknown permission" {
		t.Fatal("unknown permission line wrong")
	}
}

func TestReservedPermissionsAreNotKnown(t *testing.T) {
	for _, p := range ReservedPermissions {
		if p.Known() || !p.Reserved() {
			t.Fatalf("%s: known=%v reserved=%v", p, p.Known(), p.Reserved())
		}
	}
	for _, p := range V1Permissions {
		if !p.Known() || p.Reserved() {
			t.Fatalf("%s should be known and not reserved", p)
		}
	}
}

func TestRiskWarningText(t *testing.T) {
	want := "WARNING: Foo 1.2.3 is an UNVERIFIED community plugin.\n" +
		"It was not reviewed or signed by the Trinetra registry. It runs on this host\n" +
		"with the permissions you grant below. Only continue if you trust its publisher\n" +
		"(Acme)."
	if got := RiskWarning("Foo", "1.2.3", "Acme"); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestMissingSortedDedup(t *testing.T) {
	got := Missing([]Permission{PermMetricsRead}, []Permission{PermNetOutbound, PermMetricsRead, PermNetOutbound})
	if !reflect.DeepEqual(got, []Permission{PermNetOutbound}) {
		t.Fatalf("got %v", got)
	}
	got = Missing(nil, []Permission{PermNetOutbound, PermAlertsRead})
	if !reflect.DeepEqual(got, []Permission{PermAlertsRead, PermNetOutbound}) {
		t.Fatalf("got %v", got)
	}
}
