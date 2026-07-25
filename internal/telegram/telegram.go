package telegram

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	return &Client{
		Token:   token,
		ChatID:  chatID,
		HTTP:    &http.Client{Timeout: 65 * time.Second},
		BaseURL: "https://api.telegram.org/bot" + token,
	}
}

func (c *Client) SendMessage(text string) error {
	form := url.Values{}
	form.Set("chat_id", c.ChatID)
	form.Set("text", text)
	resp, err := c.HTTP.PostForm(c.BaseURL+"/sendMessage", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("sendMessage status %d", resp.StatusCode)
	}
	return nil
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
