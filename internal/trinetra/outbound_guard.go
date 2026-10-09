// Package trinetra: outbound_guard.go provides an opt-in SSRF guard for the
// URLs the daemon dials on the operator's behalf (webhook/Slack/Discord/ntfy/
// gotify channels and the healthchecks ping). Setting a channel URL already
// requires an admin/root config write, so this is defense-in-depth, off by
// default (notify.block_private_targets). When enabled it refuses to connect to
// loopback, unspecified, link-local (which includes the 169.254.169.254 cloud
// metadata endpoint), and private (RFC1918 / ULA) addresses.
//
// The check runs in net.Dialer.Control, i.e. AFTER DNS resolution and against
// the actual IP about to be dialed, so a hostname that resolves to an internal
// address (including a DNS-rebinding attempt) is blocked too, not just literal
// IPs in the URL.
package trinetra

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// isBlockedIP reports whether ip is an internal/loopback/link-local/private target the
// outbound guard refuses when enabled.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || // 169.254.0.0/16 (incl. 169.254.169.254) + fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsPrivate() // 10/8, 172.16/12, 192.168/16, fc00::/7
}

// guardControl is the net.Dialer.Control hook that rejects a blocked resolved
// address before the connection is made.
func guardControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); isBlockedIP(ip) {
		return fmt.Errorf("trinetra: refusing to dial blocked outbound target %s (notify.block_private_targets is on)", address)
	}
	return nil
}

// newGuardedHTTPClient returns an *http.Client bounded by timeout.
func newGuardedHTTPClient(timeout time.Duration, block bool) *http.Client {
	if !block {
		return &http.Client{Timeout: timeout}
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   guardControl,
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}
