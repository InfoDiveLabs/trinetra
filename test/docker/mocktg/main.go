// Command mocktg is a stdlib-only stand-in for the Telegram Bot API, used by the
// containerized validation harness.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// botToken extracts the "<tok>" segment from a "/bot<tok>/<method>" path, or "" if the path
// does not have that shape.
func botToken(path string) string {
	const marker = "/bot"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	if j := strings.Index(rest, "/"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// newMux builds the mock Telegram API's handler.
func newMux() http.Handler {
	var mu sync.Mutex
	var sent []string
	var sentMarkup []string
	var sentChat []string
	var answers []string
	// pending is keyed by bot token; "" is the shared/legacy bucket that
	// every getUpdates call also drains (see the package doc comment).
	pending := map[string][]map[string]any{}
	nextID := 1
	nextCallbackID := 1

	mux := http.NewServeMux()
	// Telegram-compatible endpoints (token is in the path).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "sendMessage"):
			_ = r.ParseForm()
			mu.Lock()
			sent = append(sent, r.FormValue("text"))
			sentMarkup = append(sentMarkup, r.FormValue("reply_markup"))
			sentChat = append(sentChat, r.FormValue("chat_id"))
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "answerCallbackQuery"):
			_ = r.ParseForm()
			mu.Lock()
			answers = append(answers, r.FormValue("callback_query_id")+":"+r.FormValue("text"))
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "getUpdates"):
			token := botToken(r.URL.Path)
			mu.Lock()
			var out []map[string]any
			if token == "" {
				out = pending[""]
				pending[""] = nil
			} else {
				out = append(append([]map[string]any(nil), pending[token]...), pending[""]...)
				pending[token] = nil
				pending[""] = nil
			}
			mu.Unlock()
			// Emulate a short long-poll so the daemon's poller does not spin in a tight loop when
			// there is nothing to deliver, while staying responsive to freshly injected updates.
			if len(out) == 0 {
				time.Sleep(time.Second)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	})
	mux.HandleFunc("/_inject", func(w http.ResponseWriter, r *http.Request) {
		text := r.URL.Query().Get("text")
		token := r.URL.Query().Get("token")
		mu.Lock()
		pending[token] = append(pending[token], map[string]any{
			"update_id": nextID,
			"message":   map[string]any{"text": text, "chat": map[string]any{"id": 999}},
		})
		nextID++
		mu.Unlock()
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/_inject_callback", func(w http.ResponseWriter, r *http.Request) {
		data := r.URL.Query().Get("data")
		token := r.URL.Query().Get("token")
		chat := r.URL.Query().Get("chat")
		if chat == "" {
			chat = "999"
		}
		chatID, err := strconv.ParseInt(chat, 10, 64)
		if err != nil {
			http.Error(w, "bad chat", http.StatusBadRequest)
			return
		}
		id := r.URL.Query().Get("id")
		mu.Lock()
		if id == "" {
			id = "cb" + strconv.Itoa(nextCallbackID)
			nextCallbackID++
		}
		pending[token] = append(pending[token], map[string]any{
			"update_id": nextID,
			"callback_query": map[string]any{
				"id":      id,
				"data":    data,
				"from":    map[string]any{"id": chatID},
				"message": map[string]any{"chat": map[string]any{"id": chatID}},
			},
		})
		nextID++
		mu.Unlock()
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/_messages", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(sent)
	})
	mux.HandleFunc("/_chats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(sentChat)
	})
	mux.HandleFunc("/_markups", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(sentMarkup)
	})
	mux.HandleFunc("/_answers", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(answers)
	})

	return mux
}

func main() {
	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	_ = http.ListenAndServe(addr, newMux())
}
