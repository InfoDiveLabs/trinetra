package core

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateStatusService(t *testing.T) {
	ok := StatusService{ID: "api", Name: "API", HoldDownSec: 180,
		Targets: []StatusTarget{{Kind: TargetTag, Value: "api"}}}
	if err := ValidateStatusService(ok); err != nil {
		t.Fatalf("valid service rejected: %v", err)
	}
	bad := []StatusService{
		{ID: "", Name: "API"},
		{ID: "API", Name: "API"}, // uppercase slug
		{ID: strings.Repeat("a", 41), Name: "x"},
		{ID: "api", Name: ""},
		{ID: "api", Name: strings.Repeat("n", 61)},
		{ID: "api", Name: "API", Description: strings.Repeat("d", 201)},
		{ID: "api", Name: "API", HoldDownSec: -1},
		{ID: "api", Name: "API", HoldDownSec: 3601},
		{ID: "api", Name: "API", Targets: []StatusTarget{{Kind: "bogus"}}},
		{ID: "api", Name: "API", Targets: []StatusTarget{{Kind: TargetTag}}},       // tag needs value
		{ID: "api", Name: "API", Targets: []StatusTarget{{Kind: TargetContainer}}}, // container needs value
		{ID: "api", Name: "API", Targets: []StatusTarget{{Kind: TargetNode}}},      // node needs Node
	}
	for i, s := range bad {
		if err := ValidateStatusService(s); err == nil {
			t.Errorf("case %d: %+v accepted, want error", i, s)
		}
	}
}

func TestValidateUpdateInput(t *testing.T) {
	if err := ValidateUpdate(NewUpdate{Status: IncidentInvestigating, Message: "looking"}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []NewUpdate{
		{Status: "bogus", Message: "x"},
		{Status: IncidentIdentified, Message: ""},
		{Status: IncidentIdentified, Message: "   "},
		{Status: IncidentIdentified, Message: strings.Repeat("m", MaxUpdateBytes+1)},
	} {
		if err := ValidateUpdate(u); err == nil {
			t.Errorf("%+v accepted", u)
		}
	}
}

func TestValidateNewIncident(t *testing.T) {
	good := NewIncident{Title: "API slow", Services: []string{"api"}, Impact: StateDegraded,
		Update: NewUpdate{Status: IncidentInvestigating, Message: "looking"}}
	if err := ValidateNewIncident(good); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Title = strings.Repeat("t", 201)
	if ValidateNewIncident(bad) == nil {
		t.Error("long title accepted")
	}
	bad = good
	bad.Impact = StateOperational
	if ValidateNewIncident(bad) == nil {
		t.Error("operational impact accepted")
	}
}

func TestWorseState(t *testing.T) {
	cases := []struct{ a, b, want ServiceState }{
		{StateOperational, StateDegraded, StateDegraded},
		{StateOutage, StateDegraded, StateOutage},
		{StateMaintenance, StateOperational, StateMaintenance},
		{StateMaintenance, StateDegraded, StateDegraded},
		{"", StateOperational, StateOperational},
	}
	for _, c := range cases {
		if got := WorseState(c.a, c.b); got != c.want {
			t.Errorf("WorseState(%q,%q)=%q want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestErrStatusPageOnChildIsDistinct(t *testing.T) {
	if errors.Is(ErrStatusPageOnChild, ErrNotFound) {
		t.Fatal("sentinels must be distinct")
	}
}
