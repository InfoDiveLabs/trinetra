package serverwatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"serverwatch/internal/telegram"
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
