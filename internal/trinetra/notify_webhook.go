package trinetra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/template"
	"time"
)

// webhookRequestTimeout bounds how long the underlying http.Client will wait
// for a webhook request when ctx doesn't itself carry a shorter deadline.
// The Dispatcher's per-send timeout (notifier.go) is the real backstop for
// callers going through Send, but this keeps a bare webhookNotifier from
// hanging forever if ctx has no deadline of its own.
const webhookRequestTimeout = 10 * time.Second

// defaultWebhookTemplate is used when a webhook channel doesn't configure
// its own "template" setting. It renders a single JSON object with one
// "text" field, in the spirit of the common {"text": "..."} shape used by
// Slack-/Mattermost-style incoming webhooks.
//
// Title and Body are attacker-adjacent, system-derived strings (unit names,
// container names, file paths, ...) that may legally contain quotes,
// backslashes, or newlines. text/template does no JSON-escaping on its own,
// so interpolating them directly between literal quote characters could
// produce invalid (or, worse, structurally-altered) JSON. Instead, the
// whole rendered message (severity + title, plus " -- " + body when Body is
// non-empty) is built with the "printf" builtin and piped through the
// "json" func (registered in webhookFuncMap below), which JSON-marshals the
// resulting string and emits it, quotes and all. That keeps the entire
// "text" value one properly-escaped JSON string no matter what Title/Body
// contain.
const defaultWebhookTemplate = `{"text":{{if .Body}}{{printf "%s %s -- %s" .Severity .Title .Body | json}}{{else}}{{printf "%s %s" .Severity .Title | json}}{{end}}}`

// webhookView is the data made available to a webhook body template.
type webhookView struct {
	Title    string
	Body     string
	Severity string
	// Marker is the same emoji alertMarker (notify.go) puts in front of
	// formatAlert's plain-text rendering (🚨/⚠️/ℹ️, or ✅ for a "recover"
	// Alert regardless of Severity). The slack/discord presets (presets.go)
	// use it instead of the textual Severity so their messages mirror
	// formatAlert's style; the generic default template still uses Severity
	// since it predates Marker and changing it would alter existing users'
	// webhook payloads.
	Marker string
	Kind   string
	Key    string
	Source string
	Time   int64
}

// webhookFuncMap supplies the "json" template func: it JSON-marshals its
// argument (typically a string) and returns the encoded result, including
// surrounding quotes for string values. Templates use it as
// {{.Title | json}} (or, as in defaultWebhookTemplate, on a composed
// string) to safely embed arbitrary text inside a JSON body.
//
// It also supplies "truncate", used by the Discord preset (presets.go) to
// keep the composed message under Discord's 2000-character content limit.
// Piped as {{ pipeline | truncate 1900 }}, it cuts pipeline down to at most
// n runes; slicing by rune (not byte) avoids splitting a multi-byte UTF-8
// sequence in half.
var webhookFuncMap = template.FuncMap{
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
	"truncate": func(n int, s string) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n])
	},
}

// parseWebhookTemplate parses s as a text/template body template with the
// "json" func available, returning a clear error if s is malformed.
func parseWebhookTemplate(s string) (*template.Template, error) {
	tmpl, err := template.New("webhook").Funcs(webhookFuncMap).Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse webhook template: %w", err)
	}
	return tmpl, nil
}

// webhookNotifier delivers Alerts as an HTTP request with a body rendered
// from tmpl. It's the Notifier implementation registered for the "webhook"
// ChannelConfig type in buildNotifier (channels.go).
type webhookNotifier struct {
	name        string
	url         string
	method      string
	contentType string
	tmpl        *template.Template
	// client performs the request. Defaults to a plain *http.Client with
	// webhookRequestTimeout when nil (see Send); overridable in tests.
	client *http.Client
}

func (w *webhookNotifier) Name() string { return w.name }

// Send renders a with w.tmpl and POSTs (or whatever w.method is) the result
// to w.url. It honors ctx: an already-cancelled ctx returns immediately
// without touching the network, and ctx otherwise governs the request via
// http.NewRequestWithContext. Any non-2xx response is reported as an error
// that names the status code but never the response body, since webhook
// endpoints are frequently third-party/attacker-influenced and their
// response bodies must not be trusted or leaked into logs/alerts.
func (w *webhookNotifier) Send(ctx context.Context, a Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	view := webhookView{
		Title:    a.Title,
		Body:     a.Body,
		Severity: a.Severity.String(),
		Marker:   alertMarker(a),
		Kind:     a.Kind,
		Key:      a.Key,
		Source:   a.Source,
		Time:     a.Time,
	}

	var buf bytes.Buffer
	if err := w.tmpl.Execute(&buf, view); err != nil {
		return fmt.Errorf("webhook %s: render template: %w", w.name, err)
	}

	req, err := http.NewRequestWithContext(ctx, w.method, w.url, &buf)
	if err != nil {
		return fmt.Errorf("webhook %s: build request: %w", w.name, err)
	}
	req.Header.Set("Content-Type", w.contentType)

	client := w.client
	if client == nil {
		client = &http.Client{Timeout: webhookRequestTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook %s: %w", w.name, err)
	}
	defer resp.Body.Close()
	// Drain (without retaining) so the connection can be reused; the body
	// is deliberately never surfaced in the error below.
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook %s: unexpected status %d", w.name, resp.StatusCode)
	}
	return nil
}
