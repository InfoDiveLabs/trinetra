package telegram

import (
	"context"
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
// text (4096 characters). Renderers (internal/trinetra/status.go) are
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
//
// SendMessage delegates to SendMessageContext with a background context, so
// callers that don't need cancellation get identical behavior.
func (c *Client) SendMessage(text string) error {
	return c.SendMessageContext(context.Background(), text)
}

// SendMessageContext is SendMessage's ctx-aware variant: same chunking,
// request shape, and status/error handling, but the HTTP request is built
// with http.NewRequestWithContext so a cancelled ctx aborts an in-flight
// send instead of blocking for the full client timeout (up to 65s).
func (c *Client) SendMessageContext(ctx context.Context, text string) error {
	for _, chunk := range chunkMessage(text, telegramMaxMessageLen) {
		if err := c.sendOneContext(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Button is one inline-keyboard button: Text is what the user sees, Data is
// echoed back verbatim as the resulting callback_query's Data when tapped.
// Keep Data at 64 bytes or less -- Telegram's own limit on callback_data.
type Button struct {
	Text string
	Data string
}

// SendMessageWithButtons sends text (see SendMessage) with an inline
// keyboard attached: rows renders top to bottom, each inner slice one row
// of buttons left to right. Unlike SendMessage, this never chunks -- a
// reply_markup cannot sensibly span more than one message bubble, and every
// caller (fleet_engine.go's incident-fire notifications) only ever passes
// short text. It delegates to SendMessageWithButtonsContext with a
// background context.
func (c *Client) SendMessageWithButtons(text string, rows [][]Button) error {
	return c.SendMessageWithButtonsContext(context.Background(), text, rows)
}

// SendMessageWithButtonsContext is SendMessageWithButtons' ctx-aware variant.
func (c *Client) SendMessageWithButtonsContext(ctx context.Context, text string, rows [][]Button) error {
	markup, err := json.Marshal(inlineKeyboardMarkup{InlineKeyboard: buttonRowsJSON(rows)})
	if err != nil {
		return err
	}
	return c.sendFormContext(ctx, text, string(markup))
}

// inlineKeyboardButton/inlineKeyboardMarkup mirror the Telegram Bot API's
// InlineKeyboardButton/InlineKeyboardMarkup JSON shape: marshaled, this is
// exactly the value the "reply_markup" form field carries.
type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}
type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

func buttonRowsJSON(rows [][]Button) [][]inlineKeyboardButton {
	out := make([][]inlineKeyboardButton, len(rows))
	for i, row := range rows {
		r := make([]inlineKeyboardButton, len(row))
		for j, b := range row {
			r[j] = inlineKeyboardButton{Text: b.Text, CallbackData: b.Data}
		}
		out[i] = r
	}
	return out
}

func (c *Client) sendOneContext(ctx context.Context, text string) error {
	return c.sendFormContext(ctx, text, "")
}

// sendFormContext posts one sendMessage call; replyMarkup, if non-empty, is
// the pre-marshaled JSON of an InlineKeyboardMarkup.
func (c *Client) sendFormContext(ctx context.Context, text, replyMarkup string) error {
	form := url.Values{}
	form.Set("chat_id", c.ChatID)
	form.Set("text", text)
	form.Set("parse_mode", "HTML")
	if replyMarkup != "" {
		form.Set("reply_markup", replyMarkup)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
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

// AnswerCallbackQuery acknowledges a callback query: Telegram requires this
// on every callback_query it delivers, or the tapped button's spinner never
// stops on the user's client. text, if non-empty, is shown as a brief toast.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	form := url.Values{}
	form.Set("callback_query_id", id)
	if text != "" {
		form.Set("text", text)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/answerCallbackQuery", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
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
			return fmt.Errorf("answerCallbackQuery status %d: %s", resp.StatusCode, apiErr.Description)
		}
		return fmt.Errorf("answerCallbackQuery status %d", resp.StatusCode)
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

// Update is one inbound item from GetUpdates: either a plain text message
// (Text/ChatID) or a button tap (CallbackID/CallbackData/CallbackChat) --
// never both. CallbackID is empty for a plain message.
type Update struct {
	UpdateID int
	Text     string
	ChatID   string

	// CallbackID is the callback_query's own id: AnswerCallbackQuery must be
	// called with exactly this value, or the tap's spinner never resolves.
	CallbackID string
	// CallbackData is the tapped button's Button.Data, verbatim.
	CallbackData string
	// CallbackChat is the chat the original message (carrying the inline
	// keyboard) was sent to -- the same value ChatID carries for a plain
	// message, used identically to authorize the sender.
	CallbackChat string
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
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	var ups []Update
	for _, r := range raw.Result {
		u := Update{
			UpdateID: r.UpdateID,
			Text:     strings.TrimSpace(r.Message.Text),
			ChatID:   strconv.FormatInt(r.Message.Chat.ID, 10),
		}
		if r.CallbackQuery.ID != "" {
			u.CallbackID = r.CallbackQuery.ID
			u.CallbackData = r.CallbackQuery.Data
			u.CallbackChat = strconv.FormatInt(r.CallbackQuery.Message.Chat.ID, 10)
		}
		ups = append(ups, u)
	}
	return ups, nil
}
