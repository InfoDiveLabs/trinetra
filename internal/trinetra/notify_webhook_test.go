package trinetra

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestWebhookNotifierSend(t *testing.T) {
	var gotMethod, gotContentType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tmpl, err := parseWebhookTemplate(defaultWebhookTemplate)
	if err != nil {
		t.Fatalf("parseWebhookTemplate: %v", err)
	}
	n := &webhookNotifier{
		name:        "wh",
		url:         srv.URL,
		method:      "POST",
		contentType: "application/json",
		tmpl:        tmpl,
	}

	// Title deliberately contains a quote and a newline: the default
	// template must still produce valid JSON.
	a := Alert{
		Title:    "disk \"full\"\nnow",
		Body:     "disk:/ at 95%",
		Severity: SevCritical,
		Kind:     "fire",
	}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	text, _ := decoded["text"].(string)
	if !strings.Contains(text, "critical") {
		t.Errorf("text = %q, want it to mention severity", text)
	}
	if !strings.Contains(text, `disk "full"`) {
		t.Errorf("text = %q, want it to contain the (unescaped, post-JSON-decode) title", text)
	}
	if !strings.Contains(text, "disk:/ at 95%") {
		t.Errorf("text = %q, want it to contain the body", text)
	}
}

func TestWebhookCustomTemplate(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tmpl, err := parseWebhookTemplate("{{.Kind}}:{{.Key}}:{{.Source}}")
	if err != nil {
		t.Fatalf("parseWebhookTemplate: %v", err)
	}
	n := &webhookNotifier{
		name:        "wh",
		url:         srv.URL,
		method:      "POST",
		contentType: "text/plain",
		tmpl:        tmpl,
	}

	a := Alert{Kind: "recover", Key: "disk:/", Source: "hostA"}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := "recover:disk:/:hostA"
	if string(gotBody) != want {
		t.Errorf("body = %q, want %q", gotBody, want)
	}
}

func TestWebhookNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("super secret internal stack trace"))
	}))
	defer srv.Close()

	tmpl, err := parseWebhookTemplate(defaultWebhookTemplate)
	if err != nil {
		t.Fatalf("parseWebhookTemplate: %v", err)
	}
	n := &webhookNotifier{
		name:        "wh",
		url:         srv.URL,
		method:      "POST",
		contentType: "application/json",
		tmpl:        tmpl,
	}

	err = n.Send(context.Background(), Alert{Title: "t"})
	msg := errString(t, err)
	if !strings.Contains(msg, "500") {
		t.Errorf("error = %q, want it to mention status 500", msg)
	}
	if strings.Contains(msg, "secret") {
		t.Errorf("error leaks response body: %q", msg)
	}
}

func TestWebhookCancelledCtx(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tmpl, err := parseWebhookTemplate(defaultWebhookTemplate)
	if err != nil {
		t.Fatalf("parseWebhookTemplate: %v", err)
	}
	n := &webhookNotifier{
		name:        "wh",
		url:         srv.URL,
		method:      "POST",
		contentType: "application/json",
		tmpl:        tmpl,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = n.Send(ctx, Alert{Title: "t"})
	if err == nil {
		t.Fatal("expected error for already-cancelled context")
	}
	if hit {
		t.Error("expected server not to be hit with an already-cancelled context")
	}
}

func TestBuildNotifierWebhook(t *testing.T) {
	// Missing url errors.
	if _, err := buildNotifier(config.ChannelConfig{Name: "wh", Type: "webhook"}, config.Default()); err == nil {
		t.Error("expected error for missing url")
	}

	// Bad template errors.
	if _, err := buildNotifier(config.ChannelConfig{Name: "wh", Type: "webhook", Settings: map[string]string{
		"url":      "http://example.com/hook",
		"template": "{{.Title",
	}}, config.Default()); err == nil {
		t.Error("expected error for malformed template")
	}

	// Valid config with no method/content_type/template picks the
	// documented defaults.
	cc := config.ChannelConfig{Name: "wh", Type: "webhook", Settings: map[string]string{
		"url": "http://example.com/hook",
	}}
	n, err := buildNotifier(cc, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	wn, ok := n.(*webhookNotifier)
	if !ok {
		t.Fatalf("expected *webhookNotifier, got %T", n)
	}
	if wn.url != "http://example.com/hook" {
		t.Errorf("url = %q, want http://example.com/hook", wn.url)
	}
	if wn.method != "POST" {
		t.Errorf("default method = %q, want POST", wn.method)
	}
	if wn.contentType != "application/json" {
		t.Errorf("default contentType = %q, want application/json", wn.contentType)
	}
	if wn.tmpl == nil {
		t.Error("expected a parsed default template")
	}

	// Explicit method/content_type/template are honored.
	cc2 := config.ChannelConfig{Name: "wh2", Type: "webhook", Settings: map[string]string{
		"url":          "http://example.com/hook",
		"method":       "PUT",
		"content_type": "text/plain",
		"template":     "{{.Title}}",
	}}
	n2, err := buildNotifier(cc2, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	wn2 := n2.(*webhookNotifier)
	if wn2.method != "PUT" {
		t.Errorf("method = %q, want PUT", wn2.method)
	}
	if wn2.contentType != "text/plain" {
		t.Errorf("contentType = %q, want text/plain", wn2.contentType)
	}
}
