// internal/trinetra/statuspage_cmd_test.go
package trinetra

import (
	"bytes"
	"errors"
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

// recFake records every call and can be told to fail.
type recFake struct {
	core.StatusPageAPI
	err        error
	evalErr    error
	calls      []string
	created    core.NewIncident
	actor      string
	upd        core.NewUpdate
	updID      string
	includeAll bool
	deleted    string
}

func (f *recFake) Services() ([]core.StatusService, error) {
	f.calls = append(f.calls, "services")
	return []core.StatusService{{ID: "api", Name: "API"}}, f.err
}
func (f *recFake) Evaluation() ([]core.ServiceEvaluation, error) {
	return []core.ServiceEvaluation{{ServiceID: "api", State: "outage", Reason: "pg down"}}, f.evalErr
}
func (f *recFake) DeleteService(id, actor string) error {
	f.deleted, f.actor = id, actor
	return f.err
}
func (f *recFake) Incidents(all bool) ([]core.StatusIncident, error) {
	f.includeAll = all
	return []core.StatusIncident{{ID: "inc1", Status: "investigating", Impact: "outage", Title: "DB slow"}}, f.err
}
func (f *recFake) Incident(id string) (core.StatusIncident, error) {
	return core.StatusIncident{ID: id, Title: "DB slow", Status: "identified", Impact: "outage",
		Updates: []core.IncidentUpdate{{TS: 1, Status: "identified", Message: "found it", Author: "cli"}}}, f.err
}
func (f *recFake) CreateIncident(in core.NewIncident, actor string) (core.StatusIncident, error) {
	f.created, f.actor = in, actor
	return core.StatusIncident{ID: "inc9"}, f.err
}
func (f *recFake) PostUpdate(id string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	f.updID, f.upd, f.actor = id, u, actor
	return core.StatusIncident{ID: id, Status: u.Status}, f.err
}

func withRec(t *testing.T, f *recFake) (out, errb *bytes.Buffer) {
	t.Helper()
	oldC, oldO, oldE := statusPageWithClient, stdout, stderr
	out, errb = &bytes.Buffer{}, &bytes.Buffer{}
	statusPageWithClient = func(fn func(core.StatusPageAPI) error) error { return fn(f) }
	stdout, stderr = out, errb
	t.Cleanup(func() { statusPageWithClient, stdout, stderr = oldC, oldO, oldE })
	return
}

func TestStatusPageIncidentOpen(t *testing.T) {
	f := &recFake{}
	out, _ := withRec(t, f)
	code := cmdStatusPage([]string{"incident", "open", "--title", "DB slow", "--service", "api", "--service", "web",
		"--impact", "outage", "--message", "looking", "--status", "identified"})
	if code != 0 {
		t.Fatal(code)
	}
	c := f.created
	if c.Title != "DB slow" || len(c.Services) != 2 || c.Services[1] != "web" || c.Impact != "outage" ||
		c.Update.Status != "identified" || c.Update.Message != "looking" || f.actor != "cli" {
		t.Fatalf("%+v actor=%q", c, f.actor)
	}
	if !strings.Contains(out.String(), "inc9") {
		t.Fatal(out.String())
	}
}

func TestStatusPageIncidentUpdate(t *testing.T) {
	f := &recFake{}
	withRec(t, f)
	if code := cmdStatusPage([]string{"incident", "update", "inc1", "--status", "monitoring", "--message", "watching"}); code != 0 {
		t.Fatal(code)
	}
	if f.updID != "inc1" || f.upd.Status != "monitoring" || f.upd.Message != "watching" || f.actor != "cli" {
		t.Fatalf("%+v %q %q", f.upd, f.updID, f.actor)
	}
}

func TestStatusPageIncidentShow(t *testing.T) {
	f := &recFake{}
	out, _ := withRec(t, f)
	if code := cmdStatusPage([]string{"incident", "show", "inc1"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "DB slow") || !strings.Contains(out.String(), "found it") {
		t.Fatal(out.String())
	}
}

func TestStatusPageIncidentList(t *testing.T) {
	f := &recFake{}
	out, _ := withRec(t, f)
	if code := cmdStatusPage([]string{"incident", "list"}); code != 0 || f.includeAll {
		t.Fatalf("code %d all=%v", code, f.includeAll)
	}
	if !strings.Contains(out.String(), "inc1") {
		t.Fatal(out.String())
	}
	if code := cmdStatusPage([]string{"incident", "list", "--all"}); code != 0 || !f.includeAll {
		t.Fatalf("code %d all=%v", code, f.includeAll)
	}
}

func TestStatusPageServiceListAndRm(t *testing.T) {
	f := &recFake{}
	out, _ := withRec(t, f)
	if code := cmdStatusPage([]string{"service", "list"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "pg down") {
		t.Fatal(out.String())
	}
	if code := cmdStatusPage([]string{"service", "rm", "api"}); code != 0 || f.deleted != "api" || f.actor != "cli" {
		t.Fatalf("%d %q %q", code, f.deleted, f.actor)
	}
}

func TestStatusPageServiceListEvalErrorWarns(t *testing.T) {
	f := &recFake{evalErr: errors.New("boom")}
	out, errb := withRec(t, f)
	if code := cmdStatusPage([]string{"service", "list"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "api") || !strings.Contains(errb.String(), "boom") {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}
}

func TestStatusPageUsageErrorsExit2(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"service"},
		{"bogus", "x"},
		{"service", "list", "extra"},
		{"service", "rm"},
		{"service", "rm", "a", "b"},
		{"incident", "show"},
		{"incident", "show", "a", "b"},
		{"incident", "list", "extra"},
		{"incident", "list", "-all=notabool"},
		{"incident", "list", "--bogus"},
		{"incident", "open", "extra"},
		{"incident", "update"},
		{"service", "add"},
		{"service", "add", "api", "--target", "bogus:x"},
		{"service", "add", "api", "--hold", "500ms"},
	} {
		f := &recFake{}
		withRec(t, f)
		if code := cmdStatusPage(args); code != 2 {
			t.Errorf("%v -> %d, want 2", args, code)
		}
		if f.created.Title != "" || f.deleted != "" || f.updID != "" {
			t.Errorf("%v reached the API", args)
		}
	}
}

func TestStatusPageHoldZeroAllowed(t *testing.T) {
	f := &cliStatusFake{}
	withFakeStatusPage(t, f)
	if code := cmdStatusPage([]string{"service", "add", "api", "--name", "API", "--target", "host", "--hold", "0s"}); code != 0 || f.set.HoldDownSec != 0 {
		t.Fatalf("%d %d", code, f.set.HoldDownSec)
	}
}

func TestStatusPageDaemonErrorExit1(t *testing.T) {
	for _, args := range [][]string{
		{"service", "list"},
		{"service", "rm", "api"},
		{"incident", "list"},
		{"incident", "show", "x"},
		{"incident", "open", "--title", "t", "--service", "api", "--message", "m"},
		{"incident", "resolve", "x"},
		{"service", "add", "api", "--name", "A", "--target", "host"},
	} {
		f := &recFake{err: errors.New("denied")}
		_, errb := withRec(t, f)
		if code := cmdStatusPage(args); code != 1 {
			t.Errorf("%v -> %d, want 1", args, code)
		}
		if !strings.Contains(errb.String(), "status-page: denied") {
			t.Errorf("%v stderr %q", args, errb.String())
		}
	}
}

func (f *recFake) SetService(s core.StatusService, actor string) (core.StatusService, error) {
	return s, f.err
}
