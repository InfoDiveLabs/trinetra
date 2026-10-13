package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBarWidthIsExact pins the invariant the home-screen layout relies on: bar() always
// renders exactly `width` runes regardless of percentage.
func TestBarWidthIsExact(t *testing.T) {
	for _, pct := range []float64{-10, 0, 12.5, 50, 77.7, 100, 250} {
		got := bar(pct, 10)
		if n := utf8.RuneCountInString(got); n != 10 {
			t.Errorf("bar(%v,10) = %q has %d runes, want 10", pct, got, n)
		}
	}
	if bar(50, 0) != "" {
		t.Errorf("bar with zero width should be empty")
	}
}

// TestBarFillsProportionally: 0% is all-empty, 100% is all-full, and a
// mid value is a mix (has at least one filled and one empty cell).
func TestBarFillsProportionally(t *testing.T) {
	empty := bar(0, 8)
	if strings.ContainsRune(empty, '█') {
		t.Errorf("bar(0) = %q should contain no full blocks", empty)
	}
	full := bar(100, 8)
	if strings.ContainsRune(full, '░') {
		t.Errorf("bar(100) = %q should contain no empty cells", full)
	}
	mid := bar(50, 8)
	if !strings.ContainsRune(mid, '█') || !strings.ContainsRune(mid, '░') {
		t.Errorf("bar(50) = %q should mix filled and empty", mid)
	}
}

// TestSparkUsesBlockLevels: a rising series renders rising block glyphs, the
// last (tallest) being the full block and the first the lowest.
func TestSparkUsesBlockLevels(t *testing.T) {
	got := spark([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 8)
	if utf8.RuneCountInString(got) != 8 {
		t.Fatalf("spark len = %d, want 8 (%q)", utf8.RuneCountInString(got), got)
	}
	runes := []rune(got)
	if runes[0] != '▁' {
		t.Errorf("first glyph = %q, want ▁ (lowest)", string(runes[0]))
	}
	if runes[len(runes)-1] != '█' {
		t.Errorf("last glyph = %q, want █ (highest)", string(runes[len(runes)-1]))
	}
}

// TestSparkEdgeCases: empty input renders nothing; a flat series renders
// without dividing by zero and keeps one glyph per value.
func TestSparkEdgeCases(t *testing.T) {
	if spark(nil, 8) != "" {
		t.Errorf("spark(nil) should be empty")
	}
	flat := spark([]float64{5, 5, 5}, 8)
	if utf8.RuneCountInString(flat) != 3 {
		t.Errorf("flat spark = %q, want 3 glyphs", flat)
	}
}

// TestSparkKeepsMostRecent: when there are more values than width, spark
// shows the most recent `width` of them (the tail), so the graph scrolls.
func TestSparkKeepsMostRecent(t *testing.T) {
	vals := []float64{0, 0, 0, 0, 0, 9}
	got := spark(vals, 3)
	runes := []rune(got)
	if len(runes) != 3 {
		t.Fatalf("spark len = %d, want 3", len(runes))
	}
	if runes[len(runes)-1] != '█' {
		t.Errorf("most-recent (9) should be the tallest block, got %q", got)
	}
}

// TestPaletteIsTrinetraBrand pins the TUI accents to the Trinetra palette: ember for alerts
// / down, verdigris for online, amber for warn and slate for muted text.
func TestPaletteIsTrinetraBrand(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want string
	}{
		{"colCrit (ember)", string(colCrit), "#FF5B1F"},
		{"colOK (verdigris)", string(colOK), "#3FBFA6"},
		{"colWarn (amber)", string(colWarn), "#F2B23A"},
		{"colFaint (slate)", string(colFaint), "#8A949E"},
		{"colSignal.Dark (ash)", colSignal.Dark, "#ECE8E1"},
		{"colSignal.Light (ink)", colSignal.Light, "#0E1114"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
