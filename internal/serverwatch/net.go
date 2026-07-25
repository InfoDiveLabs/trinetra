package serverwatch

import (
	"net"
	"strconv"
	"strings"
	"time"
)

var defaultConnHosts = []string{"1.1.1.1:53", "8.8.8.8:53"}

type IfaceCounters struct{ RxBytes, TxBytes uint64 }

func parseNetDev(s string) map[string]IfaceCounters {
	out := map[string]IfaceCounters{}
	for _, line := range strings.Split(s, "\n") {
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || name == "lo" || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		out[name] = IfaceCounters{RxBytes: rx, TxBytes: tx}
	}
	return out
}

func parseNetDevNames(s string) []string {
	var names []string
	for name := range parseNetDev(s) {
		names = append(names, name)
	}
	// deterministic order
	sortStrings(names)
	return names
}

func checkOnline(hosts []string, dial func(host string) bool) bool {
	for _, h := range hosts {
		if dial(h) {
			return true
		}
	}
	return false
}

func realDial(host string) bool {
	c, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
