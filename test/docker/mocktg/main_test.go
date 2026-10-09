package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestMocktgRecordsReplyMarkup pins task 9's mocktg extension: a sendMessage carrying
// reply_markup is recorded (index-aligned with /_messages).
func TestMocktgRecordsReplyMarkup(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	post(t, srv.URL+"/bot123/sendMessage", url.Values{"text": {"plain"}})
	post(t, srv.URL+"/bot123/sendMessage", url.Values{
		"text":         {"with buttons"},
		"reply_markup": {`{"inline_keyboard":[[{"text":"Ack","callback_data":"ack:abc123abc123"}]]}`},
	})

	var msgs []string
	getJSON(t, srv.URL+"/_messages", &msgs)
	if len(msgs) != 2 || msgs[0] != "plain" || msgs[1] != "with buttons" {
		t.Fatalf("/_messages = %+v", msgs)
	}
	var markups []string
	getJSON(t, srv.URL+"/_markups", &markups)
	if len(markups) != 2 {
		t.Fatalf("/_markups = %+v", markups)
	}
	if markups[0] != "" {
		t.Fatalf("markup[0] = %q, want empty (no reply_markup on that send)", markups[0])
	}
	if !strings.Contains(markups[1], "ack:abc123abc123") {
		t.Fatalf("markup[1] = %q, want it to carry the callback_data", markups[1])
	}
}

// TestMocktgCallbackInjectionRoundTrip pins the /_inject_callback -> getUpdates round trip:
// an injected callback_query is served back exactly once.
func TestMocktgCallbackInjectionRoundTrip(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/_inject_callback?data=ack%3Aabc123abc123&chat=555&id=cbq-test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var raw struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID      int `json:"update_id"`
			CallbackQuery struct {
				ID   string `json:"id"`
				Data string `json:"data"`
				From struct {
					ID int64 `json:"id"`
				} `json:"from"`
				Message struct {
					Chat struct {
						ID int64 `json:"id"`
					} `json:"chat"`
				} `json:"message"`
			} `json:"callback_query"`
		} `json:"result"`
	}
	getJSONInto(t, srv.URL+"/bot123/getUpdates", &raw)
	if len(raw.Result) != 1 {
		t.Fatalf("getUpdates result = %+v", raw.Result)
	}
	cb := raw.Result[0].CallbackQuery
	if cb.ID != "cbq-test" || cb.Data != "ack:abc123abc123" || cb.From.ID != 555 || cb.Message.Chat.ID != 555 {
		t.Fatalf("callback_query = %+v", cb)
	}

	// A second getUpdates must NOT redeliver it (drained, like plain messages already are).
	var raw2 struct {
		Result []map[string]any `json:"result"`
	}
	getJSONInto(t, srv.URL+"/bot123/getUpdates", &raw2)
	if len(raw2.Result) != 0 {
		t.Fatalf("callback_query redelivered: %+v", raw2.Result)
	}

	post(t, srv.URL+"/bot123/answerCallbackQuery", url.Values{"callback_query_id": {"cbq-test"}, "text": {"acked"}})
	var answers []string
	getJSON(t, srv.URL+"/_answers", &answers)
	if len(answers) != 1 || answers[0] != "cbq-test:acked" {
		t.Fatalf("/_answers = %+v", answers)
	}
}

// TestMocktgRecordsChatID pins the /_chats extension: chat_id is recorded
// index-aligned with /_messages, same as /_markups.
func TestMocktgRecordsChatID(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	post(t, srv.URL+"/bot123/sendMessage", url.Values{"text": {"to 999"}, "chat_id": {"999"}})
	post(t, srv.URL+"/bot123/sendMessage", url.Values{"text": {"to 222"}, "chat_id": {"222"}})

	var chats []string
	getJSON(t, srv.URL+"/_chats", &chats)
	if len(chats) != 2 || chats[0] != "999" || chats[1] != "222" {
		t.Fatalf("/_chats = %+v", chats)
	}
}

// TestMocktgGetUpdatesScopedByToken pins the fleet-e2e requirement that an update injected
// for one bot token is never delivered to a different token's getUpdates poller.
func TestMocktgGetUpdatesScopedByToken(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/_inject_callback?data=ack%3Aabc&chat=999&id=cb-a&token=tokA", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A different token's poller must see nothing.
	var otherResult struct {
		Result []map[string]any `json:"result"`
	}
	getJSONInto(t, srv.URL+"/botTokB/getUpdates", &otherResult)
	if len(otherResult.Result) != 0 {
		t.Fatalf("token B saw token A's update: %+v", otherResult.Result)
	}

	// The owning token's poller sees exactly one, and it is drained after.
	var mineResult struct {
		Result []map[string]any `json:"result"`
	}
	getJSONInto(t, srv.URL+"/bottokA/getUpdates", &mineResult)
	if len(mineResult.Result) != 1 {
		t.Fatalf("token A getUpdates = %+v, want 1", mineResult.Result)
	}
	getJSONInto(t, srv.URL+"/bottokA/getUpdates", &mineResult)
	if len(mineResult.Result) != 0 {
		t.Fatalf("token A: update redelivered: %+v", mineResult.Result)
	}

	// No token specified (legacy/shared bucket): any poller drains it.
	resp2, err := http.Post(srv.URL+"/_inject?text=hello", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	getJSONInto(t, srv.URL+"/botAnyToken/getUpdates", &otherResult)
	if len(otherResult.Result) != 1 {
		t.Fatalf("shared-bucket update not delivered: %+v", otherResult.Result)
	}
}

func post(t *testing.T, u string, form url.Values) {
	t.Helper()
	resp, err := http.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func getJSON(t *testing.T, u string, out any) {
	t.Helper()
	getJSONInto(t, u, out)
}

func getJSONInto(t *testing.T, u string, out any) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
