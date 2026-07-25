package telegram

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendMessage(t *testing.T) {
	var gotText, gotChat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotText = r.FormValue("text")
		gotChat = r.FormValue("chat_id")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL
	if err := c.SendMessage("hi"); err != nil {
		t.Fatal(err)
	}
	if gotText != "hi" || gotChat != "123" {
		t.Fatalf("text=%q chat=%q", gotText, gotChat)
	}
}

func TestGetUpdates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"ok": true,
			"result": []map[string]any{
				{"update_id": 5, "message": map[string]any{
					"text": "/stats",
					"chat": map[string]any{"id": 42},
				}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	c := New("tok", "")
	c.BaseURL = srv.URL
	ups, err := c.GetUpdates(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Text != "/stats" || ups[0].UpdateID != 5 || ups[0].ChatID != "42" {
		t.Fatalf("updates = %+v", ups)
	}
}
