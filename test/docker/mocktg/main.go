// Command mocktg is a stdlib-only stand-in for the Telegram Bot API, used by
// the containerized validation harness. It records outbound sendMessage calls
// (and their reply_markup, if any) and serves injected updates -- plain
// messages or callback queries -- back to the daemon's long poller.
//
// Endpoints:
//
//	POST /bot<tok>/sendMessage         -- records the "text"/"reply_markup" form values
//	POST /bot<tok>/answerCallbackQuery -- records the "callback_query_id"/"text" form values
//	GET  /bot<tok>/getUpdates          -- returns (and drains) injected updates
//	POST /_inject?text=...             -- test-only: queue an inbound message (chat 999)
//	POST /_inject_callback?data=...&chat=...&id=...  -- test-only: queue an inbound
//	                                       callback_query (chat/id default to 999/an
//	                                       auto-incrementing "cbN")
//	GET  /_messages  -- test-only: dump recorded sendMessage texts as a JSON array
//	GET  /_markups   -- test-only: dump recorded reply_markup values (same index as
//	                    /_messages; "" for a message sent without one) as a JSON array
//	GET  /_answers   -- test-only: dump recorded answerCallbackQuery calls, each
//	                    "<callback_query_id>:<text>", as a JSON array
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

// newMux builds the mock Telegram API's handler. Split out from main so a
// Go test can exercise it directly (over httptest) without spawning a
// subprocess or binding a real port.
func newMux() http.Handler {
	var mu sync.Mutex
	var sent []string
	var sentMarkup []string
	var answers []string
	var pending []map[string]any
	nextID := 1
	nextCallbackID := 1

	mux := http.NewServeMux()
	// Telegram-compatible endpoints (token is in the path, ignored here).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "sendMessage"):
			_ = r.ParseForm()
			mu.Lock()
			sent = append(sent, r.FormValue("text"))
			sentMarkup = append(sentMarkup, r.FormValue("reply_markup"))
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "answerCallbackQuery"):
			_ = r.ParseForm()
			mu.Lock()
			answers = append(answers, r.FormValue("callback_query_id")+":"+r.FormValue("text"))
			mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "getUpdates"):
			mu.Lock()
			out := pending
			pending = nil
			mu.Unlock()
			// Emulate a short long-poll so the daemon's poller does not spin in
			// a tight loop when there is nothing to deliver, while staying
			// responsive to freshly injected updates.
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
		mu.Lock()
		pending = append(pending, map[string]any{
			"update_id": nextID,
			"message":   map[string]any{"text": text, "chat": map[string]any{"id": 999}},
		})
		nextID++
		mu.Unlock()
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/_inject_callback", func(w http.ResponseWriter, r *http.Request) {
		data := r.URL.Query().Get("data")
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
		pending = append(pending, map[string]any{
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
