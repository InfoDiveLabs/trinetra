package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func probe(p *Prober, c Check) Result {
	return p.Run(context.Background(), c, 2*time.Second)
}

func TestHTTPCheckStatusKeywordLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(60 * time.Millisecond)
			w.Write([]byte("ready"))
		case "/gone":
			http.Error(w, "nope", http.StatusNotFound)
		case "/created":
			w.WriteHeader(http.StatusCreated)
		default:
			w.Write([]byte("hello world"))
		}
	}))
	defer srv.Close()
	p := &Prober{}

	if r := probe(p, Check{Kind: KindHTTP, Target: srv.URL}); !r.OK {
		t.Fatalf("2xx should be up: %+v", r)
	}
	if r := probe(p, Check{Kind: KindHTTP, Target: srv.URL + "/gone"}); r.OK || !strings.Contains(r.Detail, "404") {
		t.Fatalf("404 should be down mentioning 404: %+v", r)
	}
	if r := probe(p, Check{Kind: KindHTTP, Target: srv.URL + "/gone", Expect: 404}); !r.OK {
		t.Fatalf("expect=404 should be up: %+v", r)
	}
	if r := probe(p, Check{Kind: KindHTTP, Target: srv.URL + "/created", Expect: 200}); r.OK {
		t.Fatalf("201 with expect=200 should be down: %+v", r)
	}
	if r := probe(p, Check{Kind: KindHTTP, Target: srv.URL, Keyword: "world"}); !r.OK {
		t.Fatalf("keyword present should be up: %+v", r)
	}
	if r := probe(p, Check{Kind: KindHTTP, Target: srv.URL, Keyword: "absent"}); r.OK || !strings.Contains(r.Detail, "keyword") {
		t.Fatalf("keyword missing should be down: %+v", r)
	}
	r := probe(p, Check{Kind: KindHTTP, Target: srv.URL + "/slow"})
	if !r.OK || r.Latency < 50*time.Millisecond {
		t.Fatalf("latency should cover the slow handler: %+v", r)
	}
	if r.HasCert {
		t.Fatalf("plain http has no cert: %+v", r)
	}
}

func TestHTTPCheckTimeoutAndRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()
	p := &Prober{}
	r := p.Run(context.Background(), Check{Kind: KindHTTP, Target: srv.URL}, 50*time.Millisecond)
	if r.OK || r.Detail == "" {
		t.Fatalf("timeout should be down with detail: %+v", r)
	}
	addr := srv.Listener.Addr().String()
	srv.Close()
	if r := probe(p, Check{Kind: KindHTTP, Target: "http://" + addr}); r.OK {
		t.Fatalf("refused should be down: %+v", r)
	}
}

func TestHTTPKeywordOnlyReadsFirstMiB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", maxBodyRead+10)))
		w.Write([]byte("needle"))
	}))
	defer srv.Close()
	if r := probe(&Prober{}, Check{Kind: KindHTTP, Target: srv.URL, Keyword: "needle"}); r.OK {
		t.Fatalf("keyword past 1 MiB must not match: %+v", r)
	}
}

func TestTCPCheck(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	addr := ln.Addr().String()
	p := &Prober{}
	if r := probe(p, Check{Kind: KindTCP, Target: addr}); !r.OK {
		t.Fatalf("listening port should be up: %+v", r)
	}
	ln.Close()
	if r := probe(p, Check{Kind: KindTCP, Target: addr}); r.OK {
		t.Fatalf("closed port should be down: %+v", r)
	}
}

func certFor(t *testing.T, notAfter time.Time) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func tlsServer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestTLSCheckDaysLeft(t *testing.T) {
	now := time.Now()
	cert, pool := certFor(t, now.Add(10*24*time.Hour+time.Hour))
	addr := tlsServer(t, cert)
	p := &Prober{RootCAs: pool}
	r := probe(p, Check{Kind: KindTLS, Target: addr})
	if !r.OK || !r.HasCert {
		t.Fatalf("valid cert should be up with cert info: %+v", r)
	}
	if r.CertDays < 10 || r.CertDays > 11 {
		t.Fatalf("CertDays = %v, want about 10", r.CertDays)
	}
}

func TestTLSCheckExpiredIsDownButReportsDays(t *testing.T) {
	cert, pool := certFor(t, time.Now().Add(-24*time.Hour))
	addr := tlsServer(t, cert)
	r := probe(&Prober{RootCAs: pool}, Check{Kind: KindTLS, Target: addr})
	if r.OK || !r.HasCert || r.CertDays >= 0 {
		t.Fatalf("expired cert should be down with negative days: %+v", r)
	}
}

func TestTLSCheckUntrustedIsDown(t *testing.T) {
	cert, _ := certFor(t, time.Now().Add(30*24*time.Hour))
	addr := tlsServer(t, cert)
	r := probe(&Prober{RootCAs: x509.NewCertPool()}, Check{Kind: KindTLS, Target: addr})
	if r.OK || !r.HasCert {
		t.Fatalf("untrusted cert should be down but report days: %+v", r)
	}
}

func TestHTTPSCheckReportsCertDays(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cert, pool := certFor(t, time.Now().Add(20*24*time.Hour+time.Hour))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	r := probe(&Prober{RootCAs: pool}, Check{Kind: KindHTTP, Target: srv.URL})
	if !r.OK || !r.HasCert || r.CertDays < 20 || r.CertDays > 21 {
		t.Fatalf("https check should report cert days: %+v", r)
	}
}

type fakeResolver struct {
	ips   map[string][]net.IP
	cname string
	txt   []string
	err   error
}

func (f fakeResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []net.IP
	for _, ip := range f.ips[host] {
		if network == "ip4" && ip.To4() != nil || network == "ip6" && ip.To4() == nil {
			out = append(out, ip)
		}
	}
	return out, nil
}
func (f fakeResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return f.cname, f.err
}
func (f fakeResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	return f.txt, f.err
}

func TestDNSCheckWithFakeResolver(t *testing.T) {
	res := fakeResolver{
		ips:   map[string][]net.IP{"a.test": {net.ParseIP("192.0.2.7"), net.ParseIP("2001:db8::1")}},
		cname: "target.test.", txt: []string{"v=spf1 -all"},
	}
	p := &Prober{Resolver: res}
	cases := []struct {
		c  Check
		ok bool
	}{
		{Check{Kind: KindDNS, Target: "a.test", Record: "A"}, true},
		{Check{Kind: KindDNS, Target: "a.test", Record: "A", Value: "192.0.2.7"}, true},
		{Check{Kind: KindDNS, Target: "a.test", Record: "A", Value: "192.0.2.8"}, false},
		{Check{Kind: KindDNS, Target: "a.test", Record: "AAAA", Value: "2001:db8::1"}, true},
		{Check{Kind: KindDNS, Target: "none.test", Record: "A"}, false},
		{Check{Kind: KindDNS, Target: "a.test", Record: "CNAME", Value: "target.test"}, true},
		{Check{Kind: KindDNS, Target: "a.test", Record: "CNAME", Value: "other.test"}, false},
		{Check{Kind: KindDNS, Target: "a.test", Record: "TXT", Value: "spf1"}, true},
		{Check{Kind: KindDNS, Target: "a.test", Record: "TXT", Value: "dkim"}, false},
	}
	for _, c := range cases {
		if r := probe(p, c.c); r.OK != c.ok {
			t.Errorf("%+v: ok = %v, want %v (%s)", c.c, r.OK, c.ok, r.Detail)
		}
	}
	bad := &Prober{Resolver: fakeResolver{err: errors.New("servfail")}}
	if r := probe(bad, Check{Kind: KindDNS, Target: "a.test", Record: "A"}); r.OK || !strings.Contains(r.Detail, "servfail") {
		t.Fatalf("resolver error should be down: %+v", r)
	}
}
