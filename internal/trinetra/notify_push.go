package trinetra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// pushRequestTimeout bounds how long the underlying http.Client will wait for a push
// request when ctx doesn't itself carry a shorter deadline.
const pushRequestTimeout = 10 * time.Second

// defaultNtfyServer is used when an "ntfy" channel doesn't configure its own "server"
// setting: ntfy.sh is the public instance run by the ntfy project.
const defaultNtfyServer = "https://ntfy.sh"

// pushClient is the shared *http.Client used by both push notifiers,
// overridable in tests via ntfyNotifier.client / gotifyNotifier.client.
func newPushClient() *http.Client {
	return &http.Client{Timeout: pushRequestTimeout}
}

// ntfyPriority maps an Alert's Severity to ntfy's 1-5 priority scale (5 =
// max/urgent, 3 = default). See https://docs.ntfy.sh/publish/#message-priority.
func ntfyPriority(s Severity) string {
	switch s {
	case SevCritical:
		return "5"
	case SevWarning:
		return "4"
	default:
		return "3"
	}
}

// ntfyTags maps an Alert's Severity to an ntfy emoji-shortcode tag.
func ntfyTags(s Severity) string {
	switch s {
	case SevCritical:
		return "rotating_light"
	case SevWarning:
		return "warning"
	default:
		return "information_source"
	}
}

// ntfyNotifier delivers Alerts via ntfy (https://ntfy.sh, or a self-hosted instance): a
// plain HTTP POST of the message body to <server>/<topic>.
type ntfyNotifier struct {
	name   string
	server string // e.g. https://ntfy.sh (no trailing slash)
	topic  string
	token  string // optional access-token for protected topics
	// client performs the request.
	client *http.Client
}

func (n *ntfyNotifier) Name() string { return n.name }

// Send POSTs formatAlert's rendering of a as the request body to <server>/<topic>, with
// Title/Priority/Tags headers describing it.
func (n *ntfyNotifier) Send(ctx context.Context, a Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	reqURL := n.server + "/" + url.PathEscape(n.topic)
	body := formatAlert(a)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewBufferString(body))
	if err != nil {
		return fmt.Errorf("ntfy %s: build request: %w", n.name, err)
	}

	// Title is a short, single-line summary derived from system-provided Alert.Title (unit
	// names, container names, file paths, ...), which can legally contain CR/LF.
	title := sanitizeHeader(fmt.Sprintf("%s %s", a.Severity, a.Title))
	req.Header.Set("Title", title)
	req.Header.Set("Priority", ntfyPriority(a.Severity))
	req.Header.Set("Tags", ntfyTags(a.Severity))
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}

	client := n.client
	if client == nil {
		client = newPushClient()
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy %s: %w", n.name, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy %s: unexpected status %d", n.name, resp.StatusCode)
	}
	return nil
}

// gotifyPriority maps an Alert's Severity to a Gotify message priority.
func gotifyPriority(s Severity) int {
	switch s {
	case SevCritical:
		return 8
	case SevWarning:
		return 5
	default:
		return 2
	}
}

// gotifyMessage is the JSON body Gotify's message API expects.
type gotifyMessage struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority int    `json:"priority"`
}

// gotifyNotifier delivers Alerts via a self-hosted Gotify server: a JSON POST to
// <server>/message.
type gotifyNotifier struct {
	name   string
	server string // e.g. https://gotify.example.com (no trailing slash)
	token  string // Gotify application token
	// client performs the request.
	client *http.Client
}

func (g *gotifyNotifier) Name() string { return g.name }

// Send JSON-encodes a as a gotifyMessage and POSTs it to <server>/message?token=<token>.
func (g *gotifyNotifier) Send(ctx context.Context, a Alert) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	msg := gotifyMessage{
		Title:    fmt.Sprintf("%s %s", a.Severity, a.Title),
		Message:  a.Body,
		Priority: gotifyPriority(a.Severity),
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("gotify %s: encode message: %w", g.name, err)
	}

	// g.token is url.QueryEscape'd since Gotify tokens are configuration (not validated ahead
	// of time here) and could in principle contain characters.
	reqURL := fmt.Sprintf("%s/message?token=%s", g.server, url.QueryEscape(g.token))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		// err is a *url.Error that embeds reqURL (token and all) when g.server is malformed, so
		// it must not be wrapped: return a static.
		return fmt.Errorf("gotify %s: invalid server URL", g.name)
	}
	req.Header.Set("Content-Type", "application/json")

	client := g.client
	if client == nil {
		client = newPushClient()
	}

	resp, err := client.Do(req)
	if err != nil {
		// http.Client's error wraps the request URL (including the token
		// query param) via *url.Error, so it can't be returned verbatim.
		return fmt.Errorf("gotify %s: request failed", g.name)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gotify %s: unexpected status %d", g.name, resp.StatusCode)
	}
	return nil
}
