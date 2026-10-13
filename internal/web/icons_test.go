package web

import (
	"strings"
	"testing"
)

func TestEveryNavItemHasAnIcon(t *testing.T) {
	for _, e := range navItems {
		if e.Heading != "" {
			continue
		}
		if !strings.HasPrefix(string(icon(e.Icon)), "<svg") {
			t.Errorf("%s (%s): icon %q has no SVG", e.Label, e.Href, e.Icon)
		}
	}
	if icon("nope") != "" {
		t.Error("unknown icon rendered something")
	}
}
