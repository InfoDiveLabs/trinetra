package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// style.go holds the interactive TUI's visual vocabulary: the colour palette
// (kept in step with the web UI's), the two pure meter/sparkline renderers
// the home dashboard draws with, and the small status glyph helpers. The
// renderers (bar, spark) return plain runes and take no colour, so they are
// deterministic and unit-testable (style_test.go); callers wrap their output
// in one of the palette styles at draw time. lipgloss auto-detects the
// terminal's colour support and emits no ANSI when stdout is not a tty (as
// in tests), so styled home output still contains its literal label text for
// substring assertions.

// palette -- hex values mirror internal/web/assets/style.css so the terminal
// and the web dashboard read as the same product.
var (
	colOK     = lipgloss.Color("#3fb950") // healthy / online / firing-none
	colWarn   = lipgloss.Color("#d29922") // elevated
	colCrit   = lipgloss.Color("#f85149") // critical / offline / firing
	colSignal = lipgloss.Color("#f5a623") // accent (sparkline, heartbeat)
	colFaint  = lipgloss.Color("#8b949e") // secondary text
	colDim    = lipgloss.Color("#6e7681") // empty meter cells, rules
)

var (
	okStyle     = lipgloss.NewStyle().Foreground(colOK)
	warnStyle   = lipgloss.NewStyle().Foreground(colWarn)
	critStyle   = lipgloss.NewStyle().Foreground(colCrit)
	signalStyle = lipgloss.NewStyle().Foreground(colSignal)
	faintStyle  = lipgloss.NewStyle().Foreground(colFaint)
	dimStyle    = lipgloss.NewStyle().Foreground(colDim)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colDim).
			Padding(0, 1)
	panelTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(colFaint)
	labelStyle      = lipgloss.NewStyle().Foreground(colFaint)
)

// meterLevels/sparkLevels are the block glyphs used by bar() and spark().
var (
	eighths     = []rune{'▏', '▎', '▍', '▌', '▋', '▊', '▉'} // 1/8 .. 7/8 partial cell
	sparkLevels = []rune("▁▂▃▄▅▆▇█")                        // 8 heights, low to high
)

// bar renders a horizontal meter for pct (0..100) that is ALWAYS exactly
// `width` runes wide: full cells (█), one partial eighth-cell for the
// fractional remainder, then empty cells (░). Out-of-range percentages are
// clamped. width<=0 renders nothing.
func bar(pct float64, width int) string {
	if width <= 0 {
		return ""
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	units := pct / 100 * float64(width)
	full := int(units)
	if full > width {
		full = width
	}
	rem := units - float64(full)

	var b strings.Builder
	cells := 0
	for i := 0; i < full; i++ {
		b.WriteRune('█')
		cells++
	}
	if cells < width && rem > 0 {
		idx := int(rem*8) - 1
		if idx < 0 {
			idx = 0
		}
		if idx > len(eighths)-1 {
			idx = len(eighths) - 1
		}
		b.WriteRune(eighths[idx])
		cells++
	}
	for cells < width {
		b.WriteRune('░')
		cells++
	}
	return b.String()
}

// spark renders vals as a unicode block sparkline, one glyph per value,
// scaled between the series min and max. When there are more values than
// width, only the most recent `width` are shown (the graph scrolls left). A
// flat series renders at a mid height rather than dividing by zero. Empty
// input renders nothing.
func spark(vals []float64, width int) string {
	if len(vals) == 0 || width <= 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	min, max := vals[0], vals[0]
	for _, v := range vals {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	for _, v := range vals {
		idx := 3 // mid height for a flat series
		if max > min {
			idx = int((v-min)/(max-min)*7 + 0.5)
		}
		if idx < 0 {
			idx = 0
		}
		if idx > len(sparkLevels)-1 {
			idx = len(sparkLevels) - 1
		}
		b.WriteRune(sparkLevels[idx])
	}
	return b.String()
}

// meterStyle picks a palette colour for a "higher is worse" metric (CPU,
// memory, swap, disk): green under 70%, amber to 90%, red at/above 90%.
func meterStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 90:
		return critStyle
	case pct >= 70:
		return warnStyle
	default:
		return okStyle
	}
}

// onlineGlyph / onlineText render the internet-reachability indicator.
func onlineGlyph(online bool) (glyph, text string, style lipgloss.Style) {
	if online {
		return "●", "online", okStyle
	}
	return "●", "offline", critStyle
}

// severityGlyph maps an alert severity string to a coloured dot.
func severityGlyph(sev string) string {
	switch strings.ToLower(sev) {
	case "critical", "crit":
		return critStyle.Render("●")
	case "warning", "warn":
		return warnStyle.Render("●")
	default:
		return signalStyle.Render("●")
	}
}

// trunc shortens s to at most n runes, appending an ellipsis when it cuts.
func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
