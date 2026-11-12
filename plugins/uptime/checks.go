package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxBodyRead = 1 << 20

// Result is the outcome of one probe.
type Result struct {
	OK       bool
	Latency  time.Duration
	Detail   string
	HasCert  bool
	CertDays float64
	At       time.Time
}

// Resolver is the subset of net.Resolver the DNS check uses.
type Resolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupTXT(ctx context.Context, host string) ([]string, error)
}

// Prober runs checks; the zero value uses the system resolver and trust store.
type Prober struct {
	Resolver Resolver
	RootCAs  *x509.CertPool
	Now      func() time.Time
}

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) resolver() Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func daysUntil(notAfter, now time.Time) float64 { return notAfter.Sub(now).Hours() / 24 }

// Run probes c with the given timeout.
func (p *Prober) Run(ctx context.Context, c Check, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var r Result
	switch c.Kind {
	case KindHTTP:
		r = p.http(ctx, c)
	case KindTCP:
		r = p.tcp(ctx, c)
	case KindTLS:
		r = p.tls(ctx, c)
	case KindDNS:
		r = p.dns(ctx, c)
	default:
		r = Result{Detail: "unknown check kind"}
	}
	r.At = p.now()
	return r
}

func fail(start time.Time, format string, a ...any) Result {
	return Result{Latency: time.Since(start), Detail: fmt.Sprintf(format, a...)}
}

func (p *Prober) http(ctx context.Context, c Check) Result {
	start := time.Now()
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: p.RootCAs, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Target, nil)
	if err != nil {
		return fail(start, "%v", err)
	}
	req.Header.Set("User-Agent", "trinetra-uptime")
	resp, err := client.Do(req)
	if err != nil {
		return fail(start, "%s", unwrapURLError(err))
	}
	defer resp.Body.Close()
	var body []byte
	if c.Keyword != "" {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
		if err != nil {
			return fail(start, "read body: %v", err)
		}
	}
	res := Result{Latency: time.Since(start)}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		res.HasCert = true
		res.CertDays = daysUntil(resp.TLS.PeerCertificates[0].NotAfter, p.now())
	}
	okCode := resp.StatusCode >= 200 && resp.StatusCode < 300
	if c.Expect != 0 {
		okCode = resp.StatusCode == c.Expect
	}
	switch {
	case !okCode:
		res.Detail = fmt.Sprintf("status %d", resp.StatusCode)
	case c.Keyword != "" && !strings.Contains(string(body), c.Keyword):
		res.Detail = fmt.Sprintf("keyword %q not found", c.Keyword)
	default:
		res.OK = true
		res.Detail = fmt.Sprintf("status %d", resp.StatusCode)
	}
	return res
}

func unwrapURLError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func (p *Prober) tcp(ctx context.Context, c Check) Result {
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.Target)
	if err != nil {
		return fail(start, "%v", err)
	}
	conn.Close()
	return Result{OK: true, Latency: time.Since(start), Detail: "connected"}
}

func (p *Prober) tls(ctx context.Context, c Check) Result {
	start := time.Now()
	host, _, _ := net.SplitHostPort(c.Target)
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.Target)
	if err != nil {
		return fail(start, "%v", err)
	}
	defer raw.Close()
	conn := tls.Client(raw, &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err := conn.HandshakeContext(ctx); err != nil {
		return fail(start, "handshake: %v", err)
	}
	res := Result{Latency: time.Since(start)}
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		res.Detail = "no certificate presented"
		return res
	}
	now := p.now()
	res.HasCert = true
	res.CertDays = daysUntil(certs[0].NotAfter, now)
	inter := x509.NewCertPool()
	for _, ic := range certs[1:] {
		inter.AddCert(ic)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Roots: p.RootCAs, Intermediates: inter, CurrentTime: now}); err != nil {
		res.Detail = err.Error()
		return res
	}
	res.OK = true
	res.Detail = fmt.Sprintf("%.0f days left", res.CertDays)
	return res
}

func (p *Prober) dns(ctx context.Context, c Check) Result {
	start := time.Now()
	r := p.resolver()
	var got []string
	var err error
	switch c.Record {
	case "AAAA":
		got, err = lookupIPs(ctx, r, "ip6", c.Target)
	case "CNAME":
		var cn string
		cn, err = r.LookupCNAME(ctx, c.Target)
		if cn = strings.TrimSuffix(cn, "."); cn != "" {
			got = []string{cn}
		}
	case "TXT":
		got, err = r.LookupTXT(ctx, c.Target)
	default:
		got, err = lookupIPs(ctx, r, "ip4", c.Target)
	}
	if err != nil {
		return fail(start, "%v", err)
	}
	res := Result{Latency: time.Since(start)}
	if len(got) == 0 {
		res.Detail = "no " + c.Record + " records"
		return res
	}
	if c.Value != "" && !matchRecord(c, got) {
		res.Detail = fmt.Sprintf("expected %q, got %s", c.Value, strings.Join(got, ", "))
		return res
	}
	res.OK = true
	res.Detail = strings.Join(got, ", ")
	return res
}

func lookupIPs(ctx context.Context, r Resolver, network, host string) ([]string, error) {
	ips, err := r.LookupIP(ctx, network, host)
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out, err
}

func matchRecord(c Check, got []string) bool {
	for _, g := range got {
		switch c.Record {
		case "TXT":
			if strings.Contains(g, c.Value) {
				return true
			}
		case "CNAME":
			if strings.EqualFold(g, strings.TrimSuffix(c.Value, ".")) {
				return true
			}
		default:
			if want := net.ParseIP(c.Value); want != nil && want.Equal(net.ParseIP(g)) {
				return true
			}
		}
	}
	return false
}
