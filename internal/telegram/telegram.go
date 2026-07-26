package telegram

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Token, ChatID string
	HTTP          *http.Client
	BaseURL       string
}

func New(token, chatID string) *Client {
	base := "https://api.telegram.org/bot" + token
	// Test/validation override: point the client at a mock Telegram server.
	// The env var holds the host only (e.g. "http://mocktg:8080"); the
	// "/bot<token>" suffix is appended here so the mock sees the same paths
	// the real API would.
	if v := os.Getenv("TELEGRAM_BASE_URL"); v != "" {
		base = v + "/bot" + token
	}
	return &Client{
		Token:   token,
		ChatID:  chatID,
		HTTP:    &http.Client{Timeout: 65 * time.Second},
		BaseURL: base,
	}
}

// telegramMaxMessageLen is Telegram's hard limit on a single sendMessage
// text (4096 characters). Renderers (internal/serverwatch/status.go) are
// designed to stay well under this via summary-first/only-failures
// rendering, but this is the safety net for whatever still doesn't: a
// message this size used to come back as an HTTP 400 that daemon.go
// silently discarded (see fix-disk-telegram-brief.md), leaving the user
// with no reply at all.
const telegramMaxMessageLen = 4096

// SendMessage sends text as one or more Telegram messages (chunked if text
// exceeds telegramMaxMessageLen, see chunkMessage), with parse_mode=HTML so
// renderers' <pre>/<b> tags render instead of showing as literal text.
// Callers rendering dynamic content (mount names, container names, etc.)
// into HTML-mode text MUST html-escape it themselves — SendMessage does not
// re-escape, since it also carries pre-built <pre>/<b> markup that must NOT
// be escaped. If any chunk fails to send, SendMessage returns that error
// immediately (a partial multi-chunk delivery is reported, not swallowed).
func (c *Client) SendMessage(text string) error {
	for _, chunk := range chunkMessage(text, telegramMaxMessageLen) {
		if err := c.sendOne(chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) sendOne(text string) error {
	form := url.Values{}
	form.Set("chat_id", c.ChatID)
	form.Set("text", text)
	form.Set("parse_mode", "HTML")
	resp, err := c.HTTP.PostForm(c.BaseURL+"/sendMessage", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		var apiErr struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(body, &apiErr)
		if apiErr.Description != "" {
			return fmt.Errorf("sendMessage status %d: %s", resp.StatusCode, apiErr.Description)
		}
		return fmt.Errorf("sendMessage status %d", resp.StatusCode)
	}
	return nil
}

// chunkMessage splits s into parts of at most limit characters, breaking on
// newline boundaries so a single logical line is never split across two
// Telegram messages — UNLESS a single line itself exceeds limit, in which
// case that line alone is hard-split (there is no better boundary to use).
// Returns []string{s} unchanged when s already fits in one chunk (the
// common case, so callers pay nothing extra for short messages).
func chunkMessage(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	for _, line := range strings.Split(s, "\n") {
		// Hard-split a single line longer than the whole limit: there's no
		// newline to break on, so this is the one case content-within-a-line
		// gets split.
		for len(line) > limit {
			flush()
			chunks = append(chunks, line[:limit])
			line = line[limit:]
		}
		add := line
		if cur.Len() > 0 {
			add = "\n" + line
		}
		if cur.Len()+len(add) > limit {
			flush()
			add = line
		}
		cur.WriteString(add)
	}
	flush()
	return chunks
}

type Update struct {
	UpdateID int
	Text     string
	ChatID   string
}

func (c *Client) GetUpdates(offset, timeoutSec int) ([]Update, error) {
	u := fmt.Sprintf("%s/getUpdates?offset=%d&timeout=%d", c.BaseURL, offset, timeoutSec)
	resp, err := c.HTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var raw struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID int `json:"update_id"`
			Message  struct {
				Text string `json:"text"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	var ups []Update
	for _, r := range raw.Result {
		ups = append(ups, Update{
			UpdateID: r.UpdateID,
			Text:     strings.TrimSpace(r.Message.Text),
			ChatID:   strconv.FormatInt(r.Message.Chat.ID, 10),
		})
	}
	return ups, nil
}
