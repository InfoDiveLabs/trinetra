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
// into HTML-mode text MUST html-escape it themselves -- SendMessage does not
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
// Telegram messages -- UNLESS a single line itself exceeds the per-chunk
// budget, in which case that line is hard-split (there is no better boundary
// to use). Returns []string{s} unchanged when s already fits in one chunk
// (the common case, so callers pay nothing extra for short messages).
//
// Because SendMessage sends parse_mode=HTML, a chunk boundary that falls
// inside a <pre>...</pre> block would leave one chunk with an unclosed
// <pre> and the next with a stray </pre> -- unbalanced HTML that Telegram
// rejects with a 400. chunkMessage tracks <pre> nesting across the split:
// a chunk that ends still inside a block gets a synthetic </pre> appended,
// and the continuation chunk gets a synthetic <pre> prepended, so every
// emitted chunk is individually tag-balanced. (renderStatus/renderDisks
// only ever emit a single, non-nested <pre> block, which this handles;
// deeper nesting is tracked defensively but not expected.)
func chunkMessage(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	const openTag = "<pre>"
	const closeTag = "</pre>"
	var chunks []string
	var cur []string // lines buffered for the current chunk (no synthetic tags)
	curLen := 0      // len(strings.Join(cur, "\n"))
	depth := 0       // <pre> nesting after all lines consumed so far
	startedInPre := false

	// reserve is the space a chunk must leave for synthetic tags: a leading
	// "<pre>\n" if it continues a block opened earlier, and a trailing
	// "\n</pre>" if it will still be inside a block when flushed.
	reserve := func() int {
		r := 0
		if startedInPre {
			r += len(openTag) + 1
		}
		if depth > 0 {
			r += 1 + len(closeTag)
		}
		return r
	}
	emit := func(body string) {
		if startedInPre {
			body = openTag + "\n" + body
		}
		if depth > 0 {
			body = body + "\n" + closeTag
		}
		chunks = append(chunks, body)
		startedInPre = depth > 0
	}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		emit(strings.Join(cur, "\n"))
		cur = nil
		curLen = 0
	}
	addLine := func(line string) {
		depth += strings.Count(line, openTag) - strings.Count(line, closeTag)
		if depth < 0 {
			depth = 0
		}
		if len(cur) == 0 {
			curLen = len(line)
		} else {
			curLen += 1 + len(line)
		}
		cur = append(cur, line)
	}

	for _, line := range strings.Split(s, "\n") {
		// Hard-split a single line longer than the per-chunk budget: there's
		// no newline to break on. Each piece is emitted as its own balanced
		// chunk (with tag wrapping, though pre tables have short rows so this
		// path is effectively never hit for <pre> content).
		for {
			budget := limit - reserve()
			if budget < 1 {
				budget = 1
			}
			if len(line) <= budget {
				break
			}
			flush()
			piece := line[:budget]
			line = line[budget:]
			d := depth + strings.Count(piece, openTag) - strings.Count(piece, closeTag)
			if d < 0 {
				d = 0
			}
			depth = d
			emit(piece)
		}
		// Newline-boundary packing: flush if this whole line won't fit.
		joined := len(line)
		if len(cur) > 0 {
			joined = curLen + 1 + len(line)
		}
		if joined+reserve() > limit {
			flush()
		}
		addLine(line)
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
