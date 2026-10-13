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

func TestNtfyNotifierSend(t *testing.T) {
	var gotPath, gotTitle, gotPriority, gotTags string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("Title")
		gotPriority = r.Header.Get("Priority")
		gotTags = r.Header.Get("Tags")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &ntfyNotifier{name: "ntfy", server: srv.URL, topic: "myserver"}

	a := Alert{Title: "disk full", Body: "disk:/ at 95%", Severity: SevCritical, Kind: "fire"}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotPath != "/myserver" {
		t.Errorf("path = %q, want /myserver", gotPath)
	}
	if gotPriority != "5" {
		t.Errorf("Priority header = %q, want 5 (critical)", gotPriority)
	}
	if gotTags != "rotating_light" {
		t.Errorf("Tags header = %q, want rotating_light", gotTags)
	}
	if !strings.Contains(gotTitle, "critical") {
		t.Errorf("Title header = %q, want it to mention severity", gotTitle)
	}
	if !strings.Contains(string(gotBody), "disk:/ at 95%") {
		t.Errorf("body = %q, want it to contain the message", gotBody)
	}

	// A Title containing CR/LF must not produce a multi-line Title header: net/http would
	// reject/mangle a raw CR/LF in a header value.
	a2 := Alert{Title: "disk full\r\nX-Injected: evil", Body: "b", Severity: SevWarning, Kind: "fire"}
	if err := n.Send(context.Background(), a2); err != nil {
		t.Fatalf("Send (CRLF title): %v", err)
	}
	if strings.ContainsAny(gotTitle, "\r\n") {
		t.Errorf("Title header contains CR/LF: %q", gotTitle)
	}
	if gotPriority != "4" {
		t.Errorf("Priority header = %q, want 4 (warning)", gotPriority)
	}
	if gotTags != "warning" {
		t.Errorf("Tags header = %q, want warning", gotTags)
	}
}

func TestNtfyAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &ntfyNotifier{name: "ntfy", server: srv.URL, topic: "t", token: "tk-secret"}
	if err := n.Send(context.Background(), Alert{Title: "t", Severity: SevInfo, Kind: "fire"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer tk-secret" {
		t.Errorf("Authorization = %q, want Bearer tk-secret", gotAuth)
	}

	// Without a token, no Authorization header should be sent.
	var gotAuth2 string
	seen := false
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth2 = r.Header.Get("Authorization")
		seen = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv2.Close()
	n2 := &ntfyNotifier{name: "ntfy2", server: srv2.URL, topic: "t"}
	if err := n2.Send(context.Background(), Alert{Title: "t", Severity: SevInfo, Kind: "fire"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !seen {
		t.Fatal("server not hit")
	}
	if gotAuth2 != "" {
		t.Errorf("Authorization = %q, want empty when no token configured", gotAuth2)
	}
}

func TestGotifySend(t *testing.T) {
	var gotPath, gotQuery string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &gotifyNotifier{name: "gotify", server: srv.URL, token: "app-token"}

	a := Alert{Title: `disk "full"`, Body: "disk:/ at 95%", Severity: SevCritical, Kind: "fire"}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotPath != "/message" {
		t.Errorf("path = %q, want /message", gotPath)
	}
	if !strings.Contains(gotQuery, "token=app-token") {
		t.Errorf("query = %q, want token=app-token", gotQuery)
	}

	var decoded struct {
		Title    string `json:"title"`
		Message  string `json:"message"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	if decoded.Priority != 8 {
		t.Errorf("priority = %d, want 8 (critical)", decoded.Priority)
	}
	if !strings.Contains(decoded.Title, `disk "full"`) {
		t.Errorf("title = %q, want it to contain the (decoded) title", decoded.Title)
	}
	if !strings.Contains(decoded.Message, "disk:/ at 95%") {
		t.Errorf("message = %q, want it to contain the body", decoded.Message)
	}
}

func TestGotifyPriorityMapping(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &gotifyNotifier{name: "gotify", server: srv.URL, token: "tok"}

	cases := []struct {
		sev  Severity
		want int
	}{
		{SevCritical, 8},
		{SevWarning, 5},
		{SevInfo, 2},
	}
	for _, c := range cases {
		if err := n.Send(context.Background(), Alert{Title: "t", Severity: c.sev, Kind: "fire"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		var decoded struct {
			Priority int `json:"priority"`
		}
		if err := json.Unmarshal(gotBody, &decoded); err != nil {
			t.Fatalf("body not valid JSON: %v", err)
		}
		if decoded.Priority != c.want {
			t.Errorf("severity %v: priority = %d, want %d", c.sev, decoded.Priority, c.want)
		}
	}
}

func TestGotifyTokenNotInErrorString(t *testing.T) {
	// A token WITH reserved characters, so url.QueryEscape is exercised: it
	// must still not appear (raw) in any returned error string.
	const secret = "a b&c=d/e+f%g"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := &gotifyNotifier{name: "gotify", server: srv.URL, token: secret}
	err := n.Send(context.Background(), Alert{Title: "t", Severity: SevInfo, Kind: "fire"})
	msg := errString(t, err)
	if strings.Contains(msg, secret) {
		t.Errorf("error leaks token: %q", msg)
	}
}

// TestGotifyMalformedServerNoTokenLeak covers the http.NewRequestWithContext error branch:
// a malformed server URL makes url.Parse fail.
func TestGotifyMalformedServerNoTokenLeak(t *testing.T) {
	const secret = "super-secret-token"
	n := &gotifyNotifier{name: "gotify", server: "http://exa mple.com", token: secret}
	err := n.Send(context.Background(), Alert{Title: "t", Severity: SevInfo, Kind: "fire"})
	msg := errString(t, err)
	if strings.Contains(msg, secret) {
		t.Errorf("error leaks token: %q", msg)
	}
}

func TestPushCancelledCtx(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &ntfyNotifier{name: "ntfy", server: srv.URL, topic: "t"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := n.Send(ctx, Alert{Title: "t", Severity: SevInfo, Kind: "fire"})
	if err == nil {
		t.Fatal("expected error for already-cancelled context")
	}
	if hit {
		t.Error("expected server not to be hit with an already-cancelled context")
	}
}

func TestPushNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("super secret internal stack trace"))
	}))
	defer srv.Close()

	n := &ntfyNotifier{name: "ntfy", server: srv.URL, topic: "t"}
	err := n.Send(context.Background(), Alert{Title: "t", Severity: SevInfo, Kind: "fire"})
	msg := errString(t, err)
	if !strings.Contains(msg, "500") {
		t.Errorf("error = %q, want it to mention status 500", msg)
	}
	if strings.Contains(msg, "secret") {
		t.Errorf("error leaks response body: %q", msg)
	}
}

func TestBuildNotifierNtfyGotify(t *testing.T) {
	// ntfy: missing topic errors.
	if _, err := buildNotifier(config.ChannelConfig{Name: "n", Type: "ntfy"}, config.Default()); err == nil {
		t.Error("expected error for missing topic")
	}

	// ntfy: valid config with default server.
	cc := config.ChannelConfig{Name: "n", Type: "ntfy", Settings: map[string]string{"topic": "myserver"}}
	n, err := buildNotifier(cc, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	nn, ok := n.(*ntfyNotifier)
	if !ok {
		t.Fatalf("expected *ntfyNotifier, got %T", n)
	}
	if nn.server != "https://ntfy.sh" {
		t.Errorf("default server = %q, want https://ntfy.sh", nn.server)
	}
	if nn.topic != "myserver" {
		t.Errorf("topic = %q, want myserver", nn.topic)
	}

	// ntfy: explicit server + token honored.
	cc2 := config.ChannelConfig{Name: "n2", Type: "ntfy", Settings: map[string]string{
		"topic": "t", "server": "https://ntfy.example.com", "token": "tok",
	}}
	n2, err := buildNotifier(cc2, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	nn2 := n2.(*ntfyNotifier)
	if nn2.server != "https://ntfy.example.com" {
		t.Errorf("server = %q, want https://ntfy.example.com", nn2.server)
	}
	if nn2.token != "tok" {
		t.Errorf("token = %q, want tok", nn2.token)
	}

	// gotify: missing server errors.
	if _, err := buildNotifier(config.ChannelConfig{Name: "g", Type: "gotify", Settings: map[string]string{
		"token": "tok",
	}}, config.Default()); err == nil {
		t.Error("expected error for missing server")
	}

	// gotify: missing token errors.
	if _, err := buildNotifier(config.ChannelConfig{Name: "g", Type: "gotify", Settings: map[string]string{
		"server": "https://gotify.example.com",
	}}, config.Default()); err == nil {
		t.Error("expected error for missing token")
	}

	// gotify: valid config.
	cc3 := config.ChannelConfig{Name: "g", Type: "gotify", Settings: map[string]string{
		"server": "https://gotify.example.com", "token": "tok",
	}}
	n3, err := buildNotifier(cc3, config.Default())
	if err != nil {
		t.Fatalf("buildNotifier: %v", err)
	}
	gn, ok := n3.(*gotifyNotifier)
	if !ok {
		t.Fatalf("expected *gotifyNotifier, got %T", n3)
	}
	if gn.server != "https://gotify.example.com" || gn.token != "tok" {
		t.Errorf("server/token = %q/%q", gn.server, gn.token)
	}
}
