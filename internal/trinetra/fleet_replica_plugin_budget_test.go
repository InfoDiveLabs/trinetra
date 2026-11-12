package trinetra

import (
	"fmt"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

func TestReplicaPluginSeriesBudgetSeparate(t *testing.T) {
	r := newReplicaSink(t.TempDir(), StoreOptions{}, nil)
	n, err := r.node(testNodeID)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.series, n.pluginSeries = maxReplicaSeries-1, maxReplicaPluginSeries-1

	for _, tc := range []struct {
		metric string
		want   bool
	}{
		{"core.last", true},
		{"plugin.prom.last", true},
		{"plugin.prom.overflow", false},
		{"core.overflow", false},
	} {
		got, err := n.admit(tc.metric, ResRaw, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("admit(%s) = %v, want %v", tc.metric, got, tc.want)
		}
	}
	if n.series != maxReplicaSeries || n.pluginSeries != maxReplicaPluginSeries {
		t.Fatalf("series %d plugin %d", n.series, n.pluginSeries)
	}
	if n.st.DroppedCardinality != 1 || n.st.DroppedPluginCardinality != 1 {
		t.Fatalf("drops core %d plugin %d, want 1 each", n.st.DroppedCardinality, n.st.DroppedPluginCardinality)
	}
	if maxReplicaPluginSeries != 1000 {
		t.Fatalf("plugin budget = %d", maxReplicaPluginSeries)
	}
}

func TestReplicaPluginBudgetReseedCountsExisting(t *testing.T) {
	root := t.TempDir()
	r := newReplicaSink(root, StoreOptions{}, nil)
	m := map[string]float64{"cpu": 1, "mem": 2}
	for i := 0; i < 3; i++ {
		m[fmt.Sprintf("plugin.prom.m%d", i)] = float64(i)
	}
	if err := r.Apply(testNodeID, []fleet.Record{samplesRec(1, 200, m)}); err != nil {
		t.Fatal(err)
	}
	r2 := newReplicaSink(root, StoreOptions{}, nil)
	n, err := r2.node(testNodeID)
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.pluginSeries != 3 {
		t.Fatalf("pluginSeries = %d, want 3", n.pluginSeries)
	}
	if n.series != 2 {
		t.Fatalf("series = %d counted plugin series as core", n.series)
	}
	before := n.series
	if ok, _ := n.admit("plugin.prom.new", ResRaw, 300); !ok {
		t.Fatal("new plugin series refused under budget")
	}
	if n.series != before || n.pluginSeries != 4 {
		t.Fatalf("series %d plugin %d: plugin admit must not touch the core count", n.series, n.pluginSeries)
	}
}
