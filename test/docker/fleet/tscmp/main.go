// Command tscmp compares a child's raw tsfile series with the master's replica of it, for
// the fleet Docker end-to-end test (run.sh).
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
)

const (
	headerLen = 16
	recordLen = 32
)

func load(path string) ([]byte, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	if len(b) < headerLen || string(b[:4]) != "SWTS" {
		return nil, 0, fmt.Errorf("%s: not a tsfile (len %d)", path, len(b))
	}
	n := (len(b) - headerLen) / recordLen // a torn trailing record is ignored
	return b[:headerLen+n*recordLen], n, nil
}

func ts(b []byte, i int) int64 {
	off := headerLen + i*recordLen
	return int64(binary.BigEndian.Uint64(b[off : off+8]))
}

func main() {
	child := flag.String("child", "", "child's raw .tsd file")
	replica := flag.String("replica", "", "master's replica .tsd file")
	maxLag := flag.Int("maxlag", 2, "max records the replica may trail the child by")
	from := flag.Int64("from", 0, "hole-check window start (unix s, 0 = off)")
	to := flag.Int64("to", 0, "hole-check window end (unix s)")
	maxGap := flag.Int64("maxgap", 10, "max seconds between consecutive replica points in the window")
	minRecs := flag.Int("min", 1, "min records the replica must hold")
	flag.Parse()

	failCode := func(code int, format string, a ...any) {
		fmt.Printf("FAIL "+format+"\n", a...)
		os.Exit(code)
	}
	fail := func(format string, a ...any) { failCode(1, format, a...) }
	cb, cn, err := load(*child)
	if err != nil {
		fail("%v", err)
	}
	rb, rn, err := load(*replica)
	if err != nil {
		fail("%v", err)
	}
	if !bytes.Equal(cb[:headerLen], rb[:headerLen]) {
		fail("headers differ: child % x, replica % x", cb[:headerLen], rb[:headerLen])
	}
	if rn > cn {
		fail("replica has %d records, more than the child's %d", rn, cn)
	}
	if !bytes.Equal(cb[:len(rb)], rb) {
		for i := 0; i < rn; i++ {
			off := headerLen + i*recordLen
			if !bytes.Equal(cb[off:off+recordLen], rb[off:off+recordLen]) {
				fail("replica is not a prefix of the child: first difference at record %d (child ts %d, replica ts %d)", i, ts(cb, i), ts(rb, i))
			}
		}
	}
	if rn < *minRecs {
		fail("replica has only %d records (want >= %d)", rn, *minRecs)
	}
	if lag := cn - rn; lag > *maxLag {
		failCode(2, "replica lags the child by %d records (child %d, replica %d, max %d)", lag, cn, rn, *maxLag)
	}
	for i := 1; i < rn; i++ {
		if ts(rb, i) <= ts(rb, i-1) {
			fail("replica timestamps not strictly increasing at record %d (%d after %d)", i, ts(rb, i), ts(rb, i-1))
		}
	}
	gapInfo := ""
	if *from != 0 {
		if rn == 0 || ts(rb, 0) > *from || ts(rb, rn-1) < *to {
			fail("replica [%d..%d] does not span the window [%d..%d]", ts(rb, 0), ts(rb, rn-1), *from, *to)
		}
		var worst int64
		for i := 1; i < rn; i++ {
			a, b := ts(rb, i-1), ts(rb, i)
			if b < *from || a > *to {
				continue
			}
			if b-a > worst {
				worst = b - a
			}
		}
		if worst > *maxGap {
			fail("hole in the replica: %ds between points inside window [%d..%d] (max %ds)", worst, *from, *to, *maxGap)
		}
		gapInfo = fmt.Sprintf(", max gap in window %ds", worst)
	}
	fmt.Printf("ok replica is a byte prefix of the child: child %d records, replica %d (lag %d)%s\n", cn, rn, cn-rn, gapInfo)
}
