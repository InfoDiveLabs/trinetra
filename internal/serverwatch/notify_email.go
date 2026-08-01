package serverwatch

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
	"time"
)

// smtpSendFunc matches the signature of smtp.SendMail. It's a field on
// emailNotifier (rather than emailNotifier calling smtp.SendMail directly)
// so tests can stub out the network call and assert on what would have been
// sent, without standing up a live SMTP server.
type smtpSendFunc func(addr string, a smtp.Auth, from string, to []string, msg []byte) error

// emailNotifier delivers Alerts via SMTP email. It's the Notifier
// implementation registered for the "email" ChannelConfig type in
// buildNotifier (channels.go).
type emailNotifier struct {
	name     string
	host     string
	port     string
	username string
	password string
	from     string
	to       []string
	starttls bool
	// send performs the actual delivery. It defaults to smtp.SendMail (see
	// Send) when nil, which itself opportunistically upgrades to STARTTLS if
	// the server advertises support for it.
	send smtpSendFunc
}

func (e *emailNotifier) Name() string { return e.name }

// Send composes a as an RFC-822 message and delivers it via e.send. It
// honors ctx best-effort: if ctx is already cancelled it returns immediately
// without hitting the network. smtp.SendMail itself doesn't take a ctx, so
// cancellation mid-flight isn't observed here; the Dispatcher's own
// per-send timeout is the backstop for that case.
func (e *emailNotifier) Send(ctx context.Context, a Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%s", e.host, e.port)

	var auth smtp.Auth
	if e.username != "" {
		auth = smtp.PlainAuth("", e.username, e.password, e.host)
	}

	msg := buildEmailMessage(e.from, e.to, a)

	send := e.send
	if send == nil {
		send = smtp.SendMail
	}
	return send(addr, auth, e.from, e.to, msg)
}

// buildEmailMessage renders a as a plain-text RFC-822 message: From/To/
// Subject/Date headers, a blank line, then the body from formatAlert.
//
// Every value interpolated into a header line is run through
// sanitizeHeader first. Alert.Title (and, less directly, from/to) are
// system-derived -- systemd unit names, docker container names, SMART
// device paths, file names -- which can legally contain CR/LF on Linux. An
// unsanitized "\r\n" in a header value would let an attacker inject extra
// headers (Bcc:, Content-Type:) or smuggle body content (SMTP header
// injection), so CR/LF and other control bytes are stripped here.
func buildEmailMessage(from string, to []string, a Alert) []byte {
	sanitizedTo := make([]string, len(to))
	for i, addr := range to {
		sanitizedTo[i] = sanitizeHeader(addr)
	}

	subject := sanitizeHeader(fmt.Sprintf("[serverwatch] %s %s", a.Severity, a.Title))

	// Normalize the body to CRLF so the whole message is CRLF-consistent,
	// as SMTP expects.
	body := strings.ReplaceAll(formatAlert(a), "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", sanitizeHeader(from))
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(sanitizedTo, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String())
}

// sanitizeHeader makes s safe to interpolate into a single RFC-822 header
// line: every control byte below 0x20 (notably CR and LF, the SMTP
// header-injection vector) except tab is replaced with a space, then the
// result is trimmed of surrounding whitespace. This collapses any
// attempted header/body smuggling in system-derived values (unit names,
// container names, device paths, file names) into a single folded line.
func sanitizeHeader(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 && r != '\t' {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// splitEmailList parses a comma-separated recipient list, trimming
// whitespace around each address and dropping empty elements.
func splitEmailList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
