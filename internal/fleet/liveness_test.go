package fleet

import (
	"strings"
	"testing"
	"time"
)

func cfgT() TrackerConfig { return DefaultTrackerConfig(2 * time.Minute) }

func names(id string) string { return "host-" + id }

func TestTrackerStateMachine(t *testing.T) {
	tr := NewTracker(cfgT())
	tr.Seed([]string{"a"}, nil, 1000)
	if tr.State("a") != StateOnline {
		t.Fatalf("seeded state = %s", tr.State("a"))
	}
	tr.Evaluate(1031)
	if tr.State("a") != StateStale {
		t.Fatalf("after 31s = %s", tr.State("a"))
	}
	ev := tr.Evaluate(1121)
	if tr.State("a") != StateDown || len(ev.Transitions) != 1 || ev.Transitions[0].To != StateDown {
		t.Fatalf("after 121s = %s, %+v", tr.State("a"), ev)
	}
	tr.Seen("a", 1130, 0)
	ev = tr.Evaluate(1130)
	if tr.State("a") != StateOnline || ev.Transitions[0].From != StateDown {
		t.Fatalf("after contact = %s, %+v", tr.State("a"), ev)
	}
	tr.Seen("a", 1140, 600)
	tr.Evaluate(1140)
	if tr.State("a") != StateLagging {
		t.Fatalf("backlog 10m = %s", tr.State("a"))
	}
	tr.SetRevoked("a", true)
	tr.Evaluate(99999)
	if tr.State("a") != StateRevoked {
		t.Fatalf("revoked = %s", tr.State("a"))
	}
}

func TestTrackerSeedGivesGraceAfterMasterStart(t *testing.T) {
	// Master was down for an hour; on start nodes must not all be "down".
	tr := NewTracker(cfgT())
	tr.Seed([]string{"a", "b", "c", "d"}, nil, 5000)
	if ev := tr.Evaluate(5010); len(ev.Transitions) != 0 {
		t.Fatalf("transitions right after start: %+v", ev)
	}
}

func TestNodeAlerterSingleDownAndRecover(t *testing.T) {
	tr := NewTracker(cfgT())
	tr.Seed([]string{"a", "b", "c", "d", "e"}, nil, 0)
	for _, id := range []string{"b", "c", "d", "e"} {
		tr.Seen(id, 200, 0)
	}
	al := NewNodeAlerter()
	in := al.Plan(tr.Evaluate(200), 200, names)
	if len(in) != 1 || in[0].Key != "fleet:node:a:down" || !in[0].Critical || in[0].Recover || !strings.Contains(in[0].Title, "host-a") {
		t.Fatalf("intents = %+v", in)
	}
	tr.Seen("a", 300, 0)
	in = al.Plan(tr.Evaluate(300), 300, names)
	if len(in) != 1 || !in[0].Recover || in[0].Key != "fleet:node:a:down" {
		t.Fatalf("recover intents = %+v", in)
	}
}

func TestNodeAlerterMassDisconnectIsOneIncident(t *testing.T) {
	tr := NewTracker(cfgT())
	ids := []string{"a", "b", "c", "d", "e", "f"}
	tr.Seed(ids, nil, 0)
	tr.Seen("f", 125, 0) // only f still talks
	al := NewNodeAlerter()
	in := al.Plan(tr.Evaluate(125), 125, names)
	if len(in) != 1 || in[0].Key != "fleet:connectivity" || in[0].Recover {
		t.Fatalf("mass intents = %+v", in)
	}
	for _, id := range ids[:5] {
		tr.Seen(id, 200, 0)
	}
	tr.Seen("f", 200, 0)
	in = al.Plan(tr.Evaluate(200), 200, names)
	if len(in) != 1 || in[0].Key != "fleet:connectivity" || !in[0].Recover {
		t.Fatalf("mass recover intents = %+v", in)
	}
}

// TestNodeAlerterMassDisconnectAbsorbsLateStaleMembers reproduces the review
// finding: a,b,c go silent and open a mass incident; d,e,f then go silent
// too (only reaching "stale", not yet "down") while the incident is already
// open; a,b,c reconnect. The incident must NOT resolve while d,e,f are still
// lost (whether stale or, later, down), no individual down alert may ever
// fire for d,e,f, and exactly one connectivity-recover intent must fire once
// every node is finally back.
func TestNodeAlerterMassDisconnectAbsorbsLateStaleMembers(t *testing.T) {
	tr := NewTracker(cfgT())
	ids := []string{"a", "b", "c", "d", "e", "f"}
	tr.Seed(ids, nil, 0)
	al := NewNodeAlerter()

	// d,e,f are still fresh when a,b,c go down and open the incident.
	tr.Seen("d", 115, 0)
	tr.Seen("e", 115, 0)
	tr.Seen("f", 115, 0)
	in := al.Plan(tr.Evaluate(121), 121, names)
	if len(in) != 1 || in[0].Key != "fleet:connectivity" || in[0].Recover {
		t.Fatalf("open intents = %+v", in)
	}

	// d,e,f go stale (not yet down) while the incident is open: must join
	// silently, no alert emitted.
	in = al.Plan(tr.Evaluate(200), 200, names)
	if len(in) != 0 {
		t.Fatalf("stale-join intents = %+v", in)
	}

	// a,b,c reconnect while d,e,f are still stale: the incident must stay
	// open (this is exactly the bug: it used to fire an early Recover here).
	tr.Seen("a", 230, 0)
	tr.Seen("b", 230, 0)
	tr.Seen("c", 230, 0)
	in = al.Plan(tr.Evaluate(230), 230, names)
	if len(in) != 0 {
		t.Fatalf("premature intents while d/e/f still stale = %+v", in)
	}

	// d,e,f now roll from stale to down without ever being paged individually.
	in = al.Plan(tr.Evaluate(250), 250, names)
	if len(in) != 0 {
		t.Fatalf("stale->down intents for late members = %+v", in)
	}

	// Finally d,e,f reconnect: exactly one connectivity recover, no
	// individual down alert was ever raised for d, e or f.
	tr.Seen("d", 260, 0)
	tr.Seen("e", 260, 0)
	tr.Seen("f", 260, 0)
	in = al.Plan(tr.Evaluate(260), 260, names)
	if len(in) != 1 || in[0].Key != "fleet:connectivity" || !in[0].Recover {
		t.Fatalf("final recover intents = %+v", in)
	}
	for _, id := range []string{"d", "e", "f"} {
		key := "fleet:node:" + id + ":down"
		for _, intent := range in {
			if intent.Key == key {
				t.Fatalf("individual alert for late member %s: %+v", id, intent)
			}
		}
	}
}

func TestTrackerForget(t *testing.T) {
	tr := NewTracker(cfgT())
	tr.Seen("a", 100, 0)
	tr.Evaluate(100)
	tr.Forget("a")
	if s := tr.State("a"); s != "" {
		t.Fatalf("state after forget = %q", s)
	}
}

// Removing a node that is paged as down resolves its open alert instead of
// leaving it firing forever.
func TestNodeAlerterForgetResolvesOpenDown(t *testing.T) {
	tr := NewTracker(cfgT())
	tr.Seed([]string{"a"}, nil, 0)
	al := NewNodeAlerter()
	if in := al.Plan(tr.Evaluate(200), 200, names); len(in) != 1 || in[0].Recover {
		t.Fatalf("setup intents = %+v", in)
	}
	in := al.Forget("a", "host-a")
	if len(in) != 1 || !in[0].Recover || in[0].Key != "fleet:node:a:down" || !strings.Contains(in[0].Title, "host-a") {
		t.Fatalf("forget intents = %+v", in)
	}
	if in := al.Forget("a", "host-a"); len(in) != 0 {
		t.Fatalf("second forget = %+v", in)
	}
}

// Spec 5.3: a node in contact but with |clock skew| > 30s is lagging.
func TestTrackerLaggingOnClockSkew(t *testing.T) {
	tr := NewTracker(cfgT())
	tr.Seen("a", 1000, 0)
	for _, c := range []struct {
		skew int64
		want State
	}{{45, StateLagging}, {-45, StateLagging}, {30, StateOnline}, {-5, StateOnline}} {
		tr.SetSkew("a", c.skew)
		tr.Evaluate(1000)
		if got := tr.State("a"); got != c.want {
			t.Fatalf("skew %d: state %s, want %s", c.skew, got, c.want)
		}
	}
}
