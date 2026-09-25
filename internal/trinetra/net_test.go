package trinetra

import "testing"

func TestParseNetDevNames(t *testing.T) {
	s := "Inter-|   Receive ...\n face |bytes ...\n" +
		"  lo:  100 0 0\n" +
		"eth0: 5000 0 0 0 0 0 0 0 200 0\n"
	names := parseNetDevNames(s)
	if len(names) != 1 || names[0] != "eth0" {
		t.Fatalf("names = %+v", names)
	}
}

func TestParseNetDevCounters(t *testing.T) {
	s := "Inter-|   Receive |  Transmit\n face |bytes ...\n" +
		"eth0: 5000 0 0 0 0 0 0 0 200 0 0 0 0 0 0 0\n"
	m := parseNetDev(s)
	c := m["eth0"]
	if c.RxBytes != 5000 || c.TxBytes != 200 {
		t.Fatalf("counters = %+v", c)
	}
}

func TestCheckOnline(t *testing.T) {
	up := checkOnline([]string{"a", "b"}, func(h string) bool { return h == "b" })
	if !up {
		t.Fatal("should be online if any host reachable")
	}
	down := checkOnline([]string{"a", "b"}, func(h string) bool { return false })
	if down {
		t.Fatal("should be offline if none reachable")
	}
}

func TestNetRateCalcFirstCallEmpty(t *testing.T) {
	var n NetRateCalc
	rates := n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 1000, TxBytes: 500}}, 1000)
	if len(rates) != 0 {
		t.Fatalf("first call rates = %+v, want empty (no prior sample to diff against)", rates)
	}
}

func TestNetRateCalcComputesBps(t *testing.T) {
	var n NetRateCalc
	n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 1000, TxBytes: 500}}, 1000)
	rates := n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 6000, TxBytes: 1500}}, 1010)
	r, ok := rates["eth0"]
	if !ok {
		t.Fatalf("rates = %+v, want eth0 present", rates)
	}
	if r.RxBps != 500 {
		t.Errorf("RxBps = %v, want 500 ((6000-1000)/10)", r.RxBps)
	}
	if r.TxBps != 100 {
		t.Errorf("TxBps = %v, want 100 ((1500-500)/10)", r.TxBps)
	}
}

func TestNetRateCalcCounterResetSkipped(t *testing.T) {
	var n NetRateCalc
	n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 5000, TxBytes: 3000}}, 1000)
	// Counter reset/wrap: cur < prev. Must not emit a negative rate.
	rates := n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 100, TxBytes: 50}}, 1010)
	if r, ok := rates["eth0"]; ok {
		if r.RxBps < 0 || r.TxBps < 0 {
			t.Fatalf("counter reset produced negative rate: %+v", r)
		}
	}
}

func TestNetRateCalcIfaceOnlyInOneSample(t *testing.T) {
	var n NetRateCalc
	n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 1000, TxBytes: 500}}, 1000)
	// eth1 is new (not in prev); eth0 has vanished from cur.
	rates := n.Rates(map[string]IfaceCounters{"eth1": {RxBytes: 2000, TxBytes: 1000}}, 1010)
	if _, ok := rates["eth0"]; ok {
		t.Errorf("rates = %+v, want no eth0 (absent from cur)", rates)
	}
	if _, ok := rates["eth1"]; ok {
		t.Errorf("rates = %+v, want no eth1 (absent from prev)", rates)
	}
	if len(rates) != 0 {
		t.Fatalf("rates = %+v, want empty", rates)
	}
}

func TestNetRateCalcNonPositiveElapsedSkipped(t *testing.T) {
	var n NetRateCalc
	n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 1000, TxBytes: 500}}, 1000)
	// Same timestamp (elapsed == 0): must not divide by zero / emit a rate.
	rates := n.Rates(map[string]IfaceCounters{"eth0": {RxBytes: 6000, TxBytes: 1500}}, 1000)
	if _, ok := rates["eth0"]; ok {
		t.Fatalf("rates = %+v, want no eth0 when elapsed<=0", rates)
	}
}
