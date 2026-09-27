package trinetra

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

func TestTelegramNotifierSend(t *testing.T) {
	var gotText, gotChat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotText = r.FormValue("text")
		gotChat = r.FormValue("chat_id")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := telegram.New("tok", "123")
	client.BaseURL = srv.URL

	n := &telegramNotifier{client: client, name: "tg"}
	if n.Name() != "tg" {
		t.Fatalf("Name() = %q, want tg", n.Name())
	}

	a := Alert{Title: "Disk full", Body: "disk:/ at 95%", Severity: SevCritical, Kind: "fire"}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotChat != "123" {
		t.Errorf("chat_id = %q, want 123", gotChat)
	}
	want := formatAlert(a)
	if gotText != want {
		t.Errorf("text = %q, want %q", gotText, want)
	}
}

func TestTelegramNotifierSendRespectsCancelledContext(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := telegram.New("tok", "123")
	client.BaseURL = srv.URL
	n := &telegramNotifier{client: client, name: "tg"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := n.Send(ctx, Alert{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected error for already-cancelled context")
	}
	if called {
		t.Error("expected Send to bail out before hitting the server when ctx is already cancelled")
	}
}

// TestTelegramNotifierSendWithButtonsSetsMarkup pins task 9's Send wiring:
// an Alert carrying Buttons is sent with a reply_markup inline keyboard.
func TestTelegramNotifierSendWithButtonsSetsMarkup(t *testing.T) {
	var gotMarkup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotMarkup = r.FormValue("reply_markup")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := telegram.New("tok", "123")
	client.BaseURL = srv.URL
	n := &telegramNotifier{client: client, name: "tg"}

	a := Alert{Title: "node down", Kind: "fire", Buttons: [][]telegram.Button{{{Text: "Ack", Data: "ack:abc123abc123"}}}}
	if err := n.Send(context.Background(), a); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotMarkup == "" {
		t.Fatal("Alert.Buttons set but no reply_markup was sent")
	}
}

// TestTelegramNotifierSendWithoutButtonsSetsNoMarkup pins "solo/child
// behaviour unchanged": an Alert with no Buttons (every non-incident alert,
// and every alert on solo/child, which never sets Buttons at all) sends the
// exact same plain request as before this task -- no reply_markup field.
func TestTelegramNotifierSendWithoutButtonsSetsNoMarkup(t *testing.T) {
	sawMarkup := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("reply_markup") != "" {
			sawMarkup = true
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := telegram.New("tok", "123")
	client.BaseURL = srv.URL
	n := &telegramNotifier{client: client, name: "tg"}

	if err := n.Send(context.Background(), Alert{Title: "cpu high", Kind: "fire"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sawMarkup {
		t.Fatal("an Alert with no Buttons must not set reply_markup")
	}
}
