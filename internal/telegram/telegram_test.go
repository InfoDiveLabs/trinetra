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

// TestSendMessageSetsParseModeHTML: SendMessage requests HTML parsing, since
// renderers emit <pre>/<b> tags and HTML-escape dynamic content.
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

// TestSendMessageChunksLongText: a message over the 4096-char limit is split into several
// sendMessage calls, each within the limit, instead of failing with an HTTP 400.
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

// TestSendMessageReturnsErrorOnPartialChunkFailure: a failed chunk returns the
// error rather than swallowing a partial delivery.
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

// TestSendMessageIncludesAPIErrorDescription: a non-200 response's Telegram "description"
// (e.g. "Bad Request: message is too long") is folded into the returned error.
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

// TestSendMessageChunkKeepsPreBalanced asserts that when a chunk boundary falls inside a
// <pre>...</pre> block, each emitted chunk is individually tag-balanced.
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

// TestSendMessageContextCancels: SendMessageContext aborts an in-flight request
// once its ctx is cancelled instead of blocking for the HTTP client timeout.
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

// TestSendMessageWithButtonsEncodesMarkup pins the reply_markup shape
// {"inline_keyboard":[[{"text":...,"callback_data":...}]]}.
func TestSendMessageWithButtonsEncodesMarkup(t *testing.T) {
	var gotText, gotMarkup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotText = r.FormValue("text")
		gotMarkup = r.FormValue("reply_markup")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL
	rows := [][]Button{{{Text: "Ack", Data: "ack:abc123abc123"}, {Text: "Silence 1h", Data: "sil1h:abc123abc123"}}}
	if err := c.SendMessageWithButtons("node down", rows); err != nil {
		t.Fatal(err)
	}
	if gotText != "node down" {
		t.Fatalf("text = %q", gotText)
	}
	var markup struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(gotMarkup), &markup); err != nil {
		t.Fatalf("reply_markup not valid JSON: %v (%q)", err, gotMarkup)
	}
	if len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 2 {
		t.Fatalf("markup = %+v", markup)
	}
	if markup.InlineKeyboard[0][0].Text != "Ack" || markup.InlineKeyboard[0][0].CallbackData != "ack:abc123abc123" {
		t.Fatalf("button 0 = %+v", markup.InlineKeyboard[0][0])
	}
	if markup.InlineKeyboard[0][1].Text != "Silence 1h" || markup.InlineKeyboard[0][1].CallbackData != "sil1h:abc123abc123" {
		t.Fatalf("button 1 = %+v", markup.InlineKeyboard[0][1])
	}
}

// TestSendMessagePlainHasNoMarkup: the ordinary SendMessage path never sets
// reply_markup, not even an empty one.
func TestSendMessagePlainHasNoMarkup(t *testing.T) {
	var sawMarkup bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("reply_markup") != "" {
			sawMarkup = true
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL
	if err := c.SendMessage("hi"); err != nil {
		t.Fatal(err)
	}
	if sawMarkup {
		t.Fatal("plain SendMessage set reply_markup")
	}
}

// TestGetUpdatesDecodesCallbackQuery pins the callback_query shape:
// id/data/from.id/message.chat.id decode into CallbackID/CallbackData/ CallbackChat.
func TestGetUpdatesDecodesCallbackQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"ok": true,
			"result": []map[string]any{
				{"update_id": 1, "message": map[string]any{
					"text": "/status",
					"chat": map[string]any{"id": 42},
				}},
				{"update_id": 2, "callback_query": map[string]any{
					"id":   "cbq1",
					"data": "ack:abcdef012345",
					"from": map[string]any{"id": 42},
					"message": map[string]any{
						"chat": map[string]any{"id": 42},
					},
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
	if len(ups) != 2 {
		t.Fatalf("updates = %+v", ups)
	}
	if ups[0].CallbackID != "" || ups[0].Text != "/status" {
		t.Fatalf("plain message update = %+v", ups[0])
	}
	cb := ups[1]
	if cb.CallbackID != "cbq1" || cb.CallbackData != "ack:abcdef012345" || cb.CallbackChat != "42" {
		t.Fatalf("callback update = %+v", cb)
	}
}

// TestAnswerCallbackQuerySendsIDAndText pins AnswerCallbackQuery's request shape:
// callback_query_id and text as form fields against answerCallbackQuery.
func TestAnswerCallbackQuerySendsIDAndText(t *testing.T) {
	var gotPath, gotID, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotPath = r.URL.Path
		gotID = r.FormValue("callback_query_id")
		gotText = r.FormValue("text")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL
	if err := c.AnswerCallbackQuery(context.Background(), "cbq1", "acked"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "answerCallbackQuery") {
		t.Fatalf("path = %q", gotPath)
	}
	if gotID != "cbq1" || gotText != "acked" {
		t.Fatalf("id=%q text=%q", gotID, gotText)
	}
}

// TestAnswerCallbackQueryPropagatesError asserts a non-200 status is surfaced as an error
// rather than silently swallowed, matching sendOneContext's own error handling.
func TestAnswerCallbackQueryPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"description":"query is too old"}`))
	}))
	defer srv.Close()
	c := New("tok", "123")
	c.BaseURL = srv.URL
	err := c.AnswerCallbackQuery(context.Background(), "cbq1", "acked")
	if err == nil || !strings.Contains(err.Error(), "query is too old") {
		t.Fatalf("err = %v, want it to mention the API's description", err)
	}
}
