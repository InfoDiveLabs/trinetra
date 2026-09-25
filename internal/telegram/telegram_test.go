package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestSendMessageSetsParseModeHTML asserts SendMessage requests HTML
// parsing, since renderers (internal/trinetra/status.go) now emit
// <pre>/<b> tags and HTML-escape dynamic content to match.
func TestSendMessageSetsParseModeHTML(t *testing.T) {
	var gotMode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotMode = r.FormValue("parse_mode")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL
	if err := c.SendMessage("hi"); err != nil {
		t.Fatal(err)
	}
	if gotMode != "HTML" {
		t.Fatalf("parse_mode = %q, want HTML", gotMode)
	}
}

// TestSendMessageChunksLongText asserts a message over Telegram's
// 4096-char limit is split into multiple sendMessage calls, each within
// the limit, so a long overview (or a runaway failure list) can never
// silently fail with an HTTP 400 the way it used to (see
// fix-disk-telegram-brief.md).
func TestSendMessageChunksLongText(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.FormValue("text"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL

	long := strings.Repeat("x", 9000) // single line, forces a hard split
	if err := c.SendMessage(long); err != nil {
		t.Fatal(err)
	}
	if len(texts) != 3 {
		t.Fatalf("got %d sendMessage calls, want 3 (9000/4096 rounds up to 3): %v", len(texts), lens(texts))
	}
	var total int
	for _, part := range texts {
		if len(part) > 4096 {
			t.Fatalf("chunk of %d chars exceeds 4096-char limit", len(part))
		}
		total += len(part)
	}
	if total != len(long) {
		t.Fatalf("chunked total = %d chars, want %d (no content lost)", total, len(long))
	}
}

// TestSendMessageChunkNeverSplitsLine asserts chunking splits on newline
// boundaries, never mid-line, when the text has newlines to split on.
func TestSendMessageChunkNeverSplitsLine(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.FormValue("text"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL

	// 100 lines of ~50 chars = ~5000 chars, over the limit, but every line
	// is short: a correct chunker splits between lines, not inside one.
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = strings.Repeat("y", 49) + "!"
	}
	text := strings.Join(lines, "\n")
	if err := c.SendMessage(text); err != nil {
		t.Fatal(err)
	}
	if len(texts) < 2 {
		t.Fatalf("want the ~5000-char text split into multiple sends, got %d", len(texts))
	}
	for _, part := range texts {
		if len(part) > 4096 {
			t.Fatalf("chunk of %d chars exceeds 4096-char limit", len(part))
		}
		for _, l := range strings.Split(part, "\n") {
			if l != "" && l != strings.Repeat("y", 49)+"!" {
				t.Fatalf("chunk contains a partial/mangled line: %q", l)
			}
		}
	}
}

// TestSendMessageReturnsErrorOnPartialChunkFailure asserts that if any
// chunk fails to send, SendMessage returns the error (rather than
// swallowing a partial delivery).
func TestSendMessageReturnsErrorOnPartialChunkFailure(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 2 {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL

	long := strings.Repeat("x", 9000)
	if err := c.SendMessage(long); err == nil {
		t.Fatal("want an error when a chunk fails to send")
	}
}

// TestSendMessageIncludesAPIErrorDescription asserts a non-200 response's
// Telegram "description" field (e.g. "Bad Request: message is too long")
// is folded into the returned error, to help future debugging instead of
// just a bare status code.
func TestSendMessageIncludesAPIErrorDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL

	err := c.SendMessage("hi")
	if err == nil {
		t.Fatal("want an error on HTTP 400")
	}
	if !strings.Contains(err.Error(), "message is too long") {
		t.Fatalf("error = %q, want it to include Telegram's description", err.Error())
	}
}

// TestSendMessageChunkKeepsPreBalanced asserts that when a chunk boundary
// falls inside a <pre>...</pre> block, each emitted chunk is individually
// tag-balanced (the split closes </pre> and the next chunk reopens <pre>),
// so no chunk reaches Telegram as unbalanced HTML → 400.
func TestSendMessageChunkKeepsPreBalanced(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		texts = append(texts, r.FormValue("text"))
		mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL

	// A <pre>-wrapped table well over the 4096 limit: 300 lines inside one
	// <pre> block => forced to split mid-block.
	var sb strings.Builder
	sb.WriteString("<pre>\n")
	for i := 0; i < 300; i++ {
		sb.WriteString("row of table data here padded out a bit\n")
	}
	sb.WriteString("</pre>")
	if err := c.SendMessage(sb.String()); err != nil {
		t.Fatal(err)
	}
	if len(texts) < 2 {
		t.Fatalf("want the oversized <pre> table split into multiple chunks, got %d", len(texts))
	}
	for i, part := range texts {
		if len(part) > 4096 {
			t.Fatalf("chunk %d of %d chars exceeds 4096 limit", i, len(part))
		}
		if o, cl := strings.Count(part, "<pre>"), strings.Count(part, "</pre>"); o != cl {
			t.Fatalf("chunk %d has unbalanced <pre> (%d open, %d close)", i, o, cl)
		}
	}
}

// TestSendMessageContextCancels asserts SendMessageContext aborts an
// in-flight request as soon as its ctx is cancelled, rather than blocking
// for the full HTTP client timeout (up to 65s) -- the leak this task
// exists to close.
func TestSendMessageContextCancels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // slow server
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := &Client{Token: "x", ChatID: "1", BaseURL: srv.URL, HTTP: srv.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.SendMessageContext(ctx, "hi")
	if err == nil {
		t.Fatal("expected ctx cancellation error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("SendMessageContext did not honor ctx; took %s", time.Since(start))
	}
}

func lens(ss []string) []int {
	out := make([]int, len(ss))
	for i, s := range ss {
		out[i] = len(s)
	}
	return out
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
