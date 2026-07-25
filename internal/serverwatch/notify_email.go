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
func buildEmailMessage(from string, to []string, a Alert) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: [serverwatch] %s %s\r\n", a.Severity, a.Title)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("\r\n")
	b.WriteString(formatAlert(a))
	return []byte(b.String())
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
