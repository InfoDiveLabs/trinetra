package serverwatch

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
