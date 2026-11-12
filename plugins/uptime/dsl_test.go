package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseCheckDSL(t *testing.T) {
	ok := []struct {
		line string
		want Check
	}{
		{"http https://example.com/health", Check{Name: "https-example-com-health", Kind: KindHTTP, Target: "https://example.com/health"}},
		{"http http://x.test name=site interval=30s timeout=5s keyword=\"all good\" expect=204",
			Check{Name: "site", Kind: KindHTTP, Target: "http://x.test", Interval: 30 * time.Second, Timeout: 5 * time.Second, Keyword: "all good", Expect: 204}},
		{"tcp db.internal:5432", Check{Name: "db-internal-5432", Kind: KindTCP, Target: "db.internal:5432"}},
		{"tls example.com", Check{Name: "example-com-443", Kind: KindTLS, Target: "example.com:443"}},
		{"tls example.com:8443 name=Cert_A", Check{Name: "cert_a", Kind: KindTLS, Target: "example.com:8443"}},
		{"dns example.com record=TXT value=hello", Check{Name: "example-com", Kind: KindDNS, Target: "example.com", Record: "TXT", Value: "hello"}},
		{"dns example.com", Check{Name: "example-com", Kind: KindDNS, Target: "example.com", Record: "A"}},
	}
	for _, c := range ok {
		got, err := ParseCheck(c.line)
		if err != nil {
			t.Errorf("%q: %v", c.line, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q:\n got %+v\nwant %+v", c.line, got, c.want)
		}
	}

	bad := []struct {
		line string
		col  int
		msg  string
	}{
		{"", 1, "empty"},
		{"ping example.com", 1, "unknown kind"},
		{"http", 5, "target"},
		{"http ftp://x.test", 6, "http or https"},
		{"tcp example.com", 5, "host:port"},
		{"tcp a:99999", 5, "port"},
		{"http https://x.test interval=5s", 21, "at least 10s"},
		{"http https://x.test timeout=2m", 21, "at most 1m"},
		{"http https://x.test bogus=1", 21, "unknown option"},
		{"http https://x.test expect=99", 21, "expect"},
		{"tcp a:1 keyword=x", 9, "only applies to http"},
		{"http https://x.test record=A", 21, "only applies to dns"},
		{"dns example.com record=MX", 17, "record"},
		{"http https://x.test name=", 21, "empty"},
		{"http https://x.test keyword=\"open", 21, "unterminated"},
		{"http https://x.test junk", 21, "key=value"},
	}
	for _, b := range bad {
		_, err := ParseCheck(b.line)
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Errorf("%q: error = %v, want *ParseError", b.line, err)
			continue
		}
		if pe.Col != b.col || !strings.Contains(pe.Msg, b.msg) {
			t.Errorf("%q: got col %d %q, want col %d containing %q", b.line, pe.Col, pe.Msg, b.col, b.msg)
		}
	}
}

func TestSanitizeNameBounded(t *testing.T) {
	n := sanitizeName(strings.Repeat("A.b/", 40))
	if len(n) > maxNameLen || n == "" {
		t.Fatalf("name %q length %d", n, len(n))
	}
}
