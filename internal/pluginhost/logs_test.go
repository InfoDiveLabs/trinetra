package pluginhost

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestLogRateLimitDropsAndCounts(t *testing.T) {
	var lines []string
	l := NewLogSink(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }, func() time.Time { return fixedNow })
	for i := 0; i < 100; i++ {
		if err := l.Log("probe", plugin.LogEntry{Level: "info", Message: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 50 || l.Dropped("probe") != 50 {
		t.Fatalf("kept=%d dropped=%d", len(lines), l.Dropped("probe"))
	}
	if lines[0] != "plugin probe: [info] m" {
		t.Fatalf("line %q", lines[0])
	}
	if l.Dropped("other") != 0 {
		t.Fatal("counter shared")
	}
}

func TestLogStripsControlChars(t *testing.T) {
	var lines []string
	l := NewLogSink(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }, func() time.Time { return fixedNow })
	_ = l.Log("probe", plugin.LogEntry{Level: "warn", Message: "a\x1b[31mb\nc\x00d" + strings.Repeat("é", 2000)})
	if len(lines) != 1 || strings.ContainsAny(lines[0], "\x1b\n\x00") {
		t.Fatalf("%q", lines)
	}
	msg := l.Recent("probe")[0].Message
	if !strings.HasPrefix(msg, "a[31mb cd") || len([]rune(msg)) != 1024 {
		t.Fatalf("msg %q len=%d", msg[:20], len([]rune(msg)))
	}
	if err := l.Log("probe", plugin.LogEntry{Level: "trace", Message: "x"}); err == nil {
		t.Fatal("bad level accepted")
	}
}

func TestRecentRingKeepsLast50(t *testing.T) {
	now := fixedNow
	l := NewLogSink(nil, func() time.Time { return now })
	for i := 0; i < 80; i++ {
		now = now.Add(time.Second)
		_ = l.Log("probe", plugin.LogEntry{Level: "info", Message: fmt.Sprint(i)})
	}
	r := l.Recent("probe")
	if len(r) != 50 || r[0].Message != "30" || r[49].Message != "79" {
		t.Fatalf("len=%d first=%s last=%s", len(r), r[0].Message, r[len(r)-1].Message)
	}
	if r[0].Time == 0 {
		t.Fatal("time not stamped")
	}
}

func TestStatusValidatesState(t *testing.T) {
	l := NewLogSink(nil, nil)
	if err := l.SetStatus("probe", plugin.StatusReport{State: "great"}, 5); err == nil {
		t.Fatal("bad state accepted")
	}
	if err := l.SetStatus("probe", plugin.StatusReport{State: "degraded", Message: "slow"}, 9); err != nil {
		t.Fatal(err)
	}
	s, at := l.Status("probe")
	if s.State != "degraded" || s.Message != "slow" || at != 9 {
		t.Fatalf("%+v %d", s, at)
	}
	l.Forget("probe")
	if s, at := l.Status("probe"); s.State != "" || at != 0 {
		t.Fatal("not forgotten")
	}
}
