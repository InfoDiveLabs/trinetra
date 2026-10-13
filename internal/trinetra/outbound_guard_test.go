package trinetra

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},             // loopback
		{"::1", true},                   // loopback v6
		{"0.0.0.0", true},               // unspecified
		{"169.254.169.254", true},       // cloud metadata (link-local)
		{"169.254.1.1", true},           // link-local
		{"fe80::1", true},               // link-local v6
		{"10.0.0.5", true},              // private
		{"172.16.4.9", true},            // private
		{"192.168.1.20", true},          // private
		{"fc00::1", true},               // ULA (private v6)
		{"8.8.8.8", false},              // public
		{"1.1.1.1", false},              // public
		{"93.184.216.34", false},        // public (example.com)
		{"2606:4700:4700::1111", false}, // public v6
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("test bug: %q did not parse", tc.ip)
		}
		if got := isBlockedIP(ip); got != tc.want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	// A nil/unparseable IP fails closed (blocked).
	if !isBlockedIP(nil) {
		t.Error("isBlockedIP(nil) = false, want true (fail closed)")
	}
}

// TestGuardedClientBlocksLoopbackButPlainAllows drives real DNS/dial: a guarded client must
// refuse to connect to the loopback test server, while a plain client.
func TestGuardedClientBlocksLoopbackButPlainAllows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Guard OFF: the loopback server is reachable (unchanged behavior).
	plain := newGuardedHTTPClient(5*time.Second, false)
	resp, err := plain.Get(srv.URL)
	if err != nil {
		t.Fatalf("plain client (guard off) could not reach loopback test server: %v", err)
	}
	resp.Body.Close()

	// Guard ON: the same loopback URL is refused at dial time.
	guarded := newGuardedHTTPClient(5*time.Second, true)
	if _, err := guarded.Get(srv.URL); err == nil {
		t.Fatalf("guarded client reached loopback %s, want a blocked-target error", srv.URL)
	}
}

// TestBuildNotifierAppliesGuard proves the flag is wired through buildNotifier.
func TestBuildNotifierAppliesGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cc := config.ChannelConfig{Name: "wh", Type: "webhook", Settings: map[string]string{"url": srv.URL}}

	off := config.Default() // BlockPrivateTargets defaults false
	n, err := buildNotifier(cc, off)
	if err != nil {
		t.Fatalf("buildNotifier(guard off): %v", err)
	}
	if err := n.Send(context.Background(), Alert{Title: "t"}); err != nil {
		t.Fatalf("Send with guard off could not reach loopback webhook: %v", err)
	}

	on := config.Default()
	on.Notify.BlockPrivateTargets = true
	n2, err := buildNotifier(cc, on)
	if err != nil {
		t.Fatalf("buildNotifier(guard on): %v", err)
	}
	if err := n2.Send(context.Background(), Alert{Title: "t"}); err == nil {
		t.Fatal("Send with guard on reached loopback webhook, want a blocked-target error")
	}
}
