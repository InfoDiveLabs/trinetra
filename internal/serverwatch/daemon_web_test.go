//go:build web

package serverwatch

import (
	"bytes"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

// TestMaybeStartWebToleratesStartError pins the `-tags web` half of the seam
// against the failure mode issue #59 introduces: web.Start now legitimately
// returns a non-nil error (validateOrigin rejecting a passkey-unsafe/
// incomplete web.* config) even when Enabled is true. cmdDaemon (daemon.go)
// unconditionally defers the stop func maybeStartWeb returns, so a nil stop
// here would panic the whole daemon on the next shutdown — maybeStartWeb
// must instead log the error to stderr and hand back a safe no-op, letting
// the daemon itself keep running without the web UI.
func TestMaybeStartWebToleratesStartError(t *testing.T) {
	old := stderr
	var buf bytes.Buffer
	stderr = &buf
	t.Cleanup(func() { stderr = old })

	cfg := config.Default()
	cfg.Web.Mode = "manual" // no tls_cert/tls_key/rp_id/origin set -> validateOrigin fails
	stop := maybeStartWeb(WebDeps{
		Cfg:     func() *config.Config { return cfg },
		Enabled: true,
		Listen:  "127.0.0.1:0",
	})
	if stop == nil {
		t.Fatal("maybeStartWeb returned a nil stop func")
	}
	stop() // must not panic

	if !strings.Contains(buf.String(), "web") {
		t.Errorf("maybeStartWeb did not log the Start error to stderr, got: %q", buf.String())
	}
}
