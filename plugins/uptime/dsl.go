package main

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind is a check type.
type Kind string

const (
	KindHTTP Kind = "http"
	KindTCP  Kind = "tcp"
	KindTLS  Kind = "tls"
	KindDNS  Kind = "dns"
)

const (
	minInterval = 10 * time.Second
	maxTimeout  = 60 * time.Second
	maxNameLen  = 48
)

// Check is one parsed check line; zero Interval, Timeout and Expect mean "use the default".
type Check struct {
	Name     string
	Kind     Kind
	Target   string
	Interval time.Duration
	Timeout  time.Duration
	Keyword  string
	Expect   int
	Record   string
	Value    string
}

// ParseError locates a DSL problem by 1-based column.
type ParseError struct {
	Col int
	Msg string
}

func (e *ParseError) Error() string { return fmt.Sprintf("col %d: %s", e.Col, e.Msg) }

type token struct {
	text string
	col  int
}

func tokenize(line string) ([]token, error) {
	var out []token
	i := 0
	for i < len(line) {
		if line[i] == ' ' || line[i] == '\t' {
			i++
			continue
		}
		start := i
		var b strings.Builder
		quoted := false
		for i < len(line) {
			c := line[i]
			if c == '"' {
				quoted = !quoted
				i++
				continue
			}
			if !quoted && (c == ' ' || c == '\t') {
				break
			}
			b.WriteByte(c)
			i++
		}
		if quoted {
			return nil, &ParseError{Col: start + 1, Msg: "unterminated quote"}
		}
		out = append(out, token{text: b.String(), col: start + 1})
	}
	return out, nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	n := strings.Trim(b.String(), "-")
	if len(n) > maxNameLen {
		n = strings.Trim(n[:maxNameLen], "-")
	}
	return n
}

// ParseCheck parses one DSL line:
// <kind> <target> [name=n] [interval=d] [timeout=d] [keyword=t] [expect=code] [record=A|AAAA|CNAME|TXT] [value=v].
func ParseCheck(line string) (Check, error) {
	toks, err := tokenize(line)
	if err != nil {
		return Check{}, err
	}
	if len(toks) == 0 {
		return Check{}, &ParseError{Col: 1, Msg: "empty check"}
	}
	var c Check
	switch k := Kind(toks[0].text); k {
	case KindHTTP, KindTCP, KindTLS, KindDNS:
		c.Kind = k
	default:
		return Check{}, &ParseError{Col: toks[0].col, Msg: fmt.Sprintf("unknown kind %q (use http, tcp, tls or dns)", toks[0].text)}
	}
	if len(toks) < 2 {
		return Check{}, &ParseError{Col: toks[0].col + len(toks[0].text), Msg: "missing target"}
	}
	target := toks[1]
	if err := c.setTarget(target); err != nil {
		return Check{}, err
	}
	for _, t := range toks[2:] {
		if err := c.setOption(t); err != nil {
			return Check{}, err
		}
	}
	if c.Record == "" && c.Kind == KindDNS {
		c.Record = "A"
	}
	if c.Name == "" {
		c.Name = sanitizeName(c.Target)
		if c.Name == "" {
			return Check{}, &ParseError{Col: target.col, Msg: "cannot derive a name; set name="}
		}
	}
	return c, nil
}

func (c *Check) setTarget(t token) error {
	bad := func(msg string) error { return &ParseError{Col: t.col, Msg: msg} }
	c.Target = t.text
	switch c.Kind {
	case KindHTTP:
		u, err := url.Parse(t.text)
		if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
			return bad("target must be an http or https URL")
		}
	case KindTCP, KindTLS:
		host, port, err := net.SplitHostPort(t.text)
		if err != nil {
			if c.Kind == KindTLS && !strings.Contains(t.text, ":") {
				c.Target = net.JoinHostPort(t.text, "443")
				return nil
			}
			return bad("target must be host:port")
		}
		if host == "" {
			return bad("target must be host:port")
		}
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return bad("port must be 1-65535")
		}
	case KindDNS:
		if strings.ContainsAny(t.text, "/:@=") {
			return bad("target must be a host name")
		}
	}
	return nil
}

func (c *Check) setOption(t token) error {
	i := strings.IndexByte(t.text, '=')
	if i < 0 {
		return &ParseError{Col: t.col, Msg: fmt.Sprintf("%q is not key=value", t.text)}
	}
	key, val := t.text[:i], t.text[i+1:]
	bad := func(format string, a ...any) error { return &ParseError{Col: t.col, Msg: fmt.Sprintf(format, a...)} }
	if val == "" {
		return bad("%s must not be empty", key)
	}
	only := func(k Kind) error {
		if c.Kind != k {
			return bad("%s only applies to %s checks", key, k)
		}
		return nil
	}
	switch key {
	case "name":
		n := sanitizeName(val)
		if n == "" {
			return bad("name must contain letters or digits")
		}
		c.Name = n
	case "interval":
		d, err := time.ParseDuration(val)
		if err != nil {
			return bad("interval must be a duration such as 30s")
		}
		if d < minInterval {
			return bad("interval must be at least %s", minInterval)
		}
		c.Interval = d
	case "timeout":
		d, err := time.ParseDuration(val)
		if err != nil || d <= 0 {
			return bad("timeout must be a duration such as 5s")
		}
		if d > maxTimeout {
			return bad("timeout must be at most %s", maxTimeout)
		}
		c.Timeout = d
	case "keyword":
		if err := only(KindHTTP); err != nil {
			return err
		}
		c.Keyword = val
	case "expect":
		if err := only(KindHTTP); err != nil {
			return err
		}
		n, err := strconv.Atoi(val)
		if err != nil || n < 100 || n > 599 {
			return bad("expect must be an HTTP status code")
		}
		c.Expect = n
	case "record":
		if err := only(KindDNS); err != nil {
			return err
		}
		v := strings.ToUpper(val)
		switch v {
		case "A", "AAAA", "CNAME", "TXT":
			c.Record = v
		default:
			return bad("record must be A, AAAA, CNAME or TXT")
		}
	case "value":
		if err := only(KindDNS); err != nil {
			return err
		}
		c.Value = val
	default:
		return bad("unknown option %q", key)
	}
	return nil
}
