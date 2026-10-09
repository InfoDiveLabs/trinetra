package trinetra

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

// connDial is the dial function collectSlow uses for the connectivity check.
var connDial = realDial

// IfaceRate is one interface's computed throughput (bytes/sec), derived by
// NetRateCalc from two consecutive IfaceCounters samples.
type IfaceRate struct{ RxBps, TxBps float64 }

// NetRateCalc turns the cumulative counters parseNetDev reads from /proc/net/dev into
// per-interface rates, by diffing against the previous sample.
type NetRateCalc struct {
	prev   map[string]IfaceCounters
	prevTS int64
}

// Rates computes bytes/sec rates from cur against the previously stored sample, then stores
// cur/nowUnix for the next call.
func (n *NetRateCalc) Rates(cur map[string]IfaceCounters, nowUnix int64) map[string]IfaceRate {
	out := map[string]IfaceRate{}
	if n.prev != nil {
		elapsed := nowUnix - n.prevTS
		if elapsed > 0 {
			for name, c := range cur {
				p, ok := n.prev[name]
				if !ok {
					continue
				}
				if c.RxBytes < p.RxBytes || c.TxBytes < p.TxBytes {
					// counter reset/wrap: skip rather than emit a negative rate.
					continue
				}
				out[name] = IfaceRate{
					RxBps: float64(c.RxBytes-p.RxBytes) / float64(elapsed),
					TxBps: float64(c.TxBytes-p.TxBytes) / float64(elapsed),
				}
			}
		}
	}
	n.prev = cur
	n.prevTS = nowUnix
	return out
}
