// Command mocktg is a stdlib-only stand-in for the Telegram Bot API, used by
// the containerized validation harness. It records outbound sendMessage calls
// and serves injected updates back to the daemon's long poller.
//
// Endpoints:
//
//	POST /bot<tok>/sendMessage  — records the "text" form value
//	GET  /bot<tok>/getUpdates   — returns (and drains) injected updates
//	POST /_inject?text=...      — test-only: queue an inbound update (chat 999)
//	GET  /_messages             — test-only: dump recorded sends as JSON array
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	var mu sync.Mutex
	var sent []string
	var pending []map[string]any
	nextID := 1

	mux := http.NewServeMux()
	// Telegram-compatible endpoints (token is in the path, ignored here).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "sendMessage"):
			_ = r.ParseForm()
			mu.Lock()
			sent = append(sent, r.FormValue("text"))
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
	mux.HandleFunc("/_messages", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(sent)
	})

	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	_ = http.ListenAndServe(addr, mux)
}
