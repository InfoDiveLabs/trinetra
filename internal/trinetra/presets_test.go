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

func TestSlackPreset(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := buildNotifier(config.ChannelConfig{
		Name: "sl", Type: "slack",
		Settings: map[string]string{"url": srv.URL},
	}, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}

	// Title deliberately contains a quote: the preset must still produce valid JSON.
	a := Alert{
		Title:    `disk "full"`,
		Body:     "disk:/ at 95%",
		Severity: SevCritical,
		Kind:     "fire",
	}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	text, ok := decoded["text"].(string)
	if !ok {
		t.Fatalf("decoded body has no string \"text\" field: %v", decoded)
	}
	if !strings.Contains(text, alertMarker(a)) {
		t.Errorf("text = %q, want it to contain the severity marker %q", text, alertMarker(a))
	}
	if !strings.Contains(text, `disk "full"`) {
		t.Errorf("text = %q, want it to contain the (unescaped, post-JSON-decode) title", text)
	}
}

func TestDiscordPreset(t *testing.T) {
	var gotBody []byte
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := buildNotifier(config.ChannelConfig{
		Name: "dc", Type: "discord",
		Settings: map[string]string{"url": srv.URL},
	}, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}

	a := Alert{
		Title:    `disk "full"`,
		Body:     "disk:/ at 95%",
		Severity: SevWarning,
		Kind:     "fire",
	}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotContentType)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	content, ok := decoded["content"].(string)
	if !ok {
		t.Fatalf("decoded body has no string \"content\" field: %v", decoded)
	}
	if !strings.Contains(content, alertMarker(a)) {
		t.Errorf("content = %q, want it to contain the severity marker %q", content, alertMarker(a))
	}
	if !strings.Contains(content, `disk "full"`) {
		t.Errorf("content = %q, want it to contain the (unescaped, post-JSON-decode) title", content)
	}
}

func TestDiscordTruncation(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := buildNotifier(config.ChannelConfig{
		Name: "dc", Type: "discord",
		Settings: map[string]string{"url": srv.URL},
	}, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}

	a := Alert{
		Title:    strings.Repeat("T", 2000),
		Body:     strings.Repeat("B", 2000),
		Severity: SevCritical,
		Kind:     "fire",
	}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	content, _ := decoded["content"].(string)
	if len(content) > 2000 {
		t.Errorf("content length = %d, want <= 2000 (Discord's hard limit)", len(content))
	}
}

func TestBuildNotifierSlackDiscordMissingURL(t *testing.T) {
	if _, err := buildNotifier(config.ChannelConfig{Name: "sl", Type: "slack"}, config.Default()); err == nil {
		t.Error("slack: expected error for missing url")
	}
	if _, err := buildNotifier(config.ChannelConfig{Name: "dc", Type: "discord"}, config.Default()); err == nil {
		t.Error("discord: expected error for missing url")
	}
}
