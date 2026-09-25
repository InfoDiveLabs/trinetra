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
