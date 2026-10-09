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

// webhookRequestTimeout bounds how long the underlying http.Client will wait for a webhook
// request when ctx doesn't itself carry a shorter deadline.
const webhookRequestTimeout = 10 * time.Second

// defaultWebhookTemplate is used when a webhook channel doesn't configure its own
// "template" setting.
const defaultWebhookTemplate = `{"text":{{if .Body}}{{printf "%s %s -- %s" .Severity .Title .Body | json}}{{else}}{{printf "%s %s" .Severity .Title | json}}{{end}}}`

// webhookView is the data made available to a webhook body template.
type webhookView struct {
	Title    string
	Body     string
	Severity string
	// Marker is the same emoji alertMarker (notify.go) puts in front of formatAlert's
	// plain-text rendering.
	Marker string
	Kind   string
	Key    string
	Source string
	Time   int64
}

// webhookFuncMap supplies the "json" template func: it JSON-marshals its argument
// (typically a string) and returns the encoded result.
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

// webhookNotifier delivers Alerts as an HTTP request with a body rendered from tmpl.
type webhookNotifier struct {
	name        string
	url         string
	method      string
	contentType string
	tmpl        *template.Template
	// client performs the request.
	client *http.Client
}

func (w *webhookNotifier) Name() string { return w.name }

// Send renders a with w.tmpl and POSTs (or whatever w.method is) the result to w.url.
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
