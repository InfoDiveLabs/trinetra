package web

import "fmt"

// topbarStatus derives the shared topbar status pill (color class + display text) from the
// real active-alert set.
func topbarStatus(alerts []activeAlertView) (status, text string) {
	var crit, warn int
	for _, a := range alerts {
		if a.Critical {
			crit++
		} else {
			warn++
		}
	}
	switch {
	case crit > 0:
		n := crit + warn
		return "crit", fmt.Sprintf("%d alert%s firing", n, plural(n))
	case warn > 0:
		return "warn", fmt.Sprintf("%d warning%s", warn, plural(warn))
	default:
		return "ok", "All systems normal"
	}
}

// plural returns "s" for any count but exactly 1, for topbarStatus's
// alert/warning counts ("1 alert firing" vs "2 alerts firing").
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
