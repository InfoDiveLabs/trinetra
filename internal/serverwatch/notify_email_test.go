package serverwatch

import (
	"context"
	"net/smtp"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

func TestEmailNotifierSend(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	var gotAuth smtp.Auth

	stub := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotAddr = addr
		gotAuth = a
		gotFrom = from
		gotTo = to
		gotMsg = msg
		return nil
	}

	n := &emailNotifier{
		name:     "mail",
		host:     "smtp.example.com",
		port:     "587",
		username: "user",
		password: "pass",
		from:     "alerts@example.com",
		to:       []string{"a@example.com", "b@example.com"},
		starttls: true,
		send:     stub,
	}

	if n.Name() != "mail" {
		t.Fatalf("Name() = %q, want mail", n.Name())
	}

	a := Alert{Title: "Disk full", Body: "disk:/ at 95%", Severity: SevCritical, Kind: "fire"}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotAddr != "smtp.example.com:587" {
		t.Errorf("addr = %q, want smtp.example.com:587", gotAddr)
	}
	if gotFrom != "alerts@example.com" {
		t.Errorf("from = %q, want alerts@example.com", gotFrom)
	}
	if len(gotTo) != 2 || gotTo[0] != "a@example.com" || gotTo[1] != "b@example.com" {
		t.Errorf("to = %v", gotTo)
	}
	if gotAuth == nil {
		t.Error("expected non-nil auth when username is set")
	}

	msg := string(gotMsg)
	wantSubject := "Subject: [serverwatch] critical Disk full"
	if !strings.Contains(msg, wantSubject) {
		t.Errorf("msg missing subject line %q, got:\n%s", wantSubject, msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com") {
		t.Errorf("msg missing To header, got:\n%s", msg)
	}
	if !strings.Contains(msg, "From: alerts@example.com") {
		t.Errorf("msg missing From header, got:\n%s", msg)
	}
	// The body is the formatAlert output with line endings normalized to
	// CRLF for SMTP consistency.
	wantBody := strings.ReplaceAll(formatAlert(a), "\n", "\r\n")
	if !strings.Contains(msg, wantBody) {
		t.Errorf("msg missing formatAlert body, got:\n%s", msg)
	}
}

func TestEmailNotifierSendNoAuthWithoutUsername(t *testing.T) {
	var gotAuth smtp.Auth
	authSeen := false
	stub := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotAuth = a
		authSeen = true
		return nil
	}
	n := &emailNotifier{
		name: "mail", host: "smtp.example.com", port: "587",
		from: "alerts@example.com", to: []string{"a@example.com"}, send: stub,
	}
	if err := n.Send(context.Background(), Alert{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !authSeen {
		t.Fatal("send func not called")
	}
	if gotAuth != nil {
		t.Errorf("expected nil auth without username, got %v", gotAuth)
	}
}

func TestEmailHeaderInjection(t *testing.T) {
	var gotMsg []byte
	stub := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotMsg = msg
		return nil
	}
	n := &emailNotifier{
		name: "mail", host: "h", port: "587",
		from: "alerts@example.com", to: []string{"a@example.com"}, send: stub,
	}

	// Title is system-derived and can legally contain CR/LF on Linux; a
	// malicious one must not be able to inject headers or body content.
	a := Alert{Title: "disk full\r\nBcc: evil@example.com", Body: "b", Kind: "fire"}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg := string(gotMsg)

	// The injection surface is the header block (everything before the
	// blank line that separates headers from body). The body legitimately
	// reflects the Title, so "Bcc:"/evil@ appearing there is harmless text,
	// not a smuggled header — scope the assertions to the header block.
	headers, _, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatalf("message has no header/body separator:\n%s", msg)
	}
	// No header LINE may be a Bcc: the CRLF injected via Title must have been
	// stripped, so "Bcc: evil@example.com" can only survive (harmlessly) as
	// mid-line text folded into the Subject value, never as its own header.
	subjectLines := 0
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Errorf("header injection: a header line is a Bcc header: %q", line)
		}
		if strings.HasPrefix(line, "Subject:") {
			subjectLines++
		}
	}
	if subjectLines != 1 {
		t.Errorf("expected exactly 1 Subject line, got %d:\n%s", subjectLines, headers)
	}
}

func TestEmailNotifierCancelledCtx(t *testing.T) {
	called := false
	stub := func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		called = true
		return nil
	}
	n := &emailNotifier{
		name: "mail", host: "h", port: "587",
		from: "f@example.com", to: []string{"t@example.com"}, send: stub,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := n.Send(ctx, Alert{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected error for already-cancelled context")
	}
	if called {
		t.Error("expected Send to bail out before calling send when ctx is already cancelled")
	}
}

func TestBuildNotifierEmail(t *testing.T) {
	cc := config.ChannelConfig{Name: "mail", Type: "email", Settings: map[string]string{
		"host": "smtp.example.com",
		"from": "alerts@example.com",
		"to":   "a@example.com, b@example.com",
	}}
	n, err := buildNotifier(cc, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	if n.Name() != "mail" {
		t.Errorf("Name() = %q, want mail", n.Name())
	}
	en, ok := n.(*emailNotifier)
	if !ok {
		t.Fatalf("expected *emailNotifier, got %T", n)
	}
	if en.port != "587" {
		t.Errorf("default port = %q, want 587", en.port)
	}
	if !en.starttls {
		t.Error("expected starttls default true")
	}
	if len(en.to) != 2 || en.to[0] != "a@example.com" || en.to[1] != "b@example.com" {
		t.Errorf("to = %v, want [a@example.com b@example.com]", en.to)
	}

	// Explicit non-default port and starttls=false are honored.
	cc2 := config.ChannelConfig{Name: "mail2", Type: "email", Settings: map[string]string{
		"host": "smtp.example.com", "from": "a@example.com", "to": "b@example.com",
		"port": "25", "starttls": "false", "username": "u", "password": "p",
	}}
	n2, err := buildNotifier(cc2, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	en2 := n2.(*emailNotifier)
	if en2.port != "25" {
		t.Errorf("port = %q, want 25", en2.port)
	}
	if en2.starttls {
		t.Error("expected starttls false")
	}
	if en2.username != "u" || en2.password != "p" {
		t.Errorf("username/password = %q/%q", en2.username, en2.password)
	}

	// Missing host, from, to each produce a distinct, clearly-attributed error.
	_, err = buildNotifier(config.ChannelConfig{Name: "x", Type: "email", Settings: map[string]string{
		"from": "a@example.com", "to": "b@example.com",
	}}, config.Default())
	hostErr := errString(t, err)
	if !strings.Contains(hostErr, "host") {
		t.Errorf("missing-host error = %q, want it to mention host", hostErr)
	}

	_, err = buildNotifier(config.ChannelConfig{Name: "x", Type: "email", Settings: map[string]string{
		"host": "smtp.example.com", "to": "b@example.com",
	}}, config.Default())
	fromErr := errString(t, err)
	if !strings.Contains(fromErr, "from") {
		t.Errorf("missing-from error = %q, want it to mention from", fromErr)
	}

	_, err = buildNotifier(config.ChannelConfig{Name: "x", Type: "email", Settings: map[string]string{
		"host": "smtp.example.com", "from": "a@example.com",
	}}, config.Default())
	toErr := errString(t, err)
	if !strings.Contains(toErr, "to") {
		t.Errorf("missing-to error = %q, want it to mention to", toErr)
	}

	if hostErr == fromErr || fromErr == toErr || hostErr == toErr {
		t.Error("expected distinct error messages per missing field")
	}
}

// errString asserts err is non-nil and returns its message, failing the
// test if it's unexpectedly nil.
func errString(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	return err.Error()
}
