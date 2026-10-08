// internal/trinetra/statuspage_cmd_test.go
package trinetra

import (
	"bytes"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func TestParseStatusTarget(t *testing.T) {
	cases := map[string]core.StatusTarget{
		"host":               {Kind: core.TargetHost},
		"node:abc":           {Kind: core.TargetNode, Node: "abc"},
		"tag:api":            {Kind: core.TargetTag, Value: "api"},
		"container:pg":       {Kind: core.TargetContainer, Value: "pg"},
		"container:pg@n1":    {Kind: core.TargetContainer, Value: "pg", Node: "n1"},
		"unit:nginx.service": {Kind: core.TargetUnit, Value: "nginx.service"},
		"mount:/data@n2":     {Kind: core.TargetMount, Value: "/data", Node: "n2"},
	}
	for in, want := range cases {
		got, err := parseStatusTarget(in)
		if err != nil || got != want {
			t.Errorf("%q -> %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "bogus:x", "tag:", "node:"} {
		if _, err := parseStatusTarget(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

type cliStatusFake struct {
	core.StatusPageAPI
	set     core.StatusService
	updates []core.NewUpdate
	actor   string
}

func (f *cliStatusFake) SetService(s core.StatusService, actor string) (core.StatusService, error) {
	f.set, f.actor = s, actor
	return s, nil
}
func (f *cliStatusFake) PostUpdate(id string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	f.updates = append(f.updates, u)
	f.actor = actor
	return core.StatusIncident{ID: id, Status: u.Status}, nil
}

func withFakeStatusPage(t *testing.T, f *cliStatusFake) *bytes.Buffer {
	t.Helper()
	old, oldOut := statusPageWithClient, stdout
	var buf bytes.Buffer
	statusPageWithClient = func(fn func(core.StatusPageAPI) error) error { return fn(f) }
	stdout = &buf
	t.Cleanup(func() { statusPageWithClient, stdout = old, oldOut })
	return &buf
}

func TestStatusPageServiceAdd(t *testing.T) {
	f := &cliStatusFake{}
	out := withFakeStatusPage(t, f)
	code := cmdStatusPage([]string{"service", "add", "api", "--name", "API", "--hold", "2m", "--target", "tag:api", "--target", "container:pg@n1"})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if f.set.Name != "API" || f.set.HoldDownSec != 120 || len(f.set.Targets) != 2 || f.actor != "cli" {
		t.Fatalf("%+v actor=%q", f.set, f.actor)
	}
	if !strings.Contains(out.String(), "api") {
		t.Fatalf("output %q", out.String())
	}
}

func TestStatusPageServiceAddDefaultsHoldDown(t *testing.T) {
	f := &cliStatusFake{}
	withFakeStatusPage(t, f)
	if code := cmdStatusPage([]string{"service", "add", "api", "--name", "API", "--target", "host"}); code != 0 {
		t.Fatal(code)
	}
	if f.set.HoldDownSec != core.DefaultHoldDownSec {
		t.Fatalf("hold %d", f.set.HoldDownSec)
	}
}

func TestStatusPageIncidentResolveDefaultMessage(t *testing.T) {
	f := &cliStatusFake{}
	withFakeStatusPage(t, f)
	if code := cmdStatusPage([]string{"incident", "resolve", "inc1"}); code != 0 {
		t.Fatal(code)
	}
	if len(f.updates) != 1 || f.updates[0].Status != core.IncidentResolved || f.updates[0].Message != "This incident has been resolved." {
		t.Fatalf("%+v", f.updates)
	}
}
