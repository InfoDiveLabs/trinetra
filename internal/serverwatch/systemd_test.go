package serverwatch

import (
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	u := renderUnit("/usr/local/bin/serverwatch")
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"ExecStart=/usr/local/bin/serverwatch daemon",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit missing %q:\n%s", want, u)
		}
	}
}
