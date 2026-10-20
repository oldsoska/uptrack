package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"time"
)

// Telegram sends a message through the Bot API (https://core.telegram.org/bots/api#sendmessage).
type Telegram struct {
	Token   string
	ChatID  string
	BaseURL string // https://api.telegram.org; overridden in tests
	Client  *http.Client
}

func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{
		Token: token, ChatID: chatID, BaseURL: "https://api.telegram.org",
		Client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (t *Telegram) Notify(ctx context.Context, e Event) error {
	icon := "🔴"
	if e.Kind == KindRecovered {
		icon = "🟢"
	}
	text := fmt.Sprintf("%s %s\n%s", icon, e.Summary(), e.Monitor.URL)
	body, _ := json.Marshal(map[string]any{
		"chat_id":                  t.ChatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	// The token is part of the path; it must never end up in logs, so errors below omit the URL.
	return post(ctx, t.Client, t.BaseURL+"/bot"+t.Token+"/sendMessage", body, "telegram")
}

// Webhook POSTs the event as JSON, e.g. to Slack-compatible or custom automation endpoints.
type Webhook struct {
	URL    string
	Client *http.Client
}

func NewWebhook(url string) *Webhook {
	return &Webhook{URL: url, Client: &http.Client{Timeout: 10 * time.Second}}
}

type webhookPayload struct {
	Event      Kind       `json:"event"`
	Summary    string     `json:"summary"`
	MonitorID  int64      `json:"monitorId"`
	Monitor    string     `json:"monitor"`
	URL        string     `json:"url"`
	IncidentID int64      `json:"incidentId"`
	Cause      string     `json:"cause"`
	OpenedAt   time.Time  `json:"openedAt"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

func (w *Webhook) Notify(ctx context.Context, e Event) error {
	body, err := json.Marshal(webhookPayload{
		Event: e.Kind, Summary: e.Summary(), MonitorID: e.Monitor.ID, Monitor: e.Monitor.Name, URL: e.Monitor.URL,
		IncidentID: e.Incident.ID, Cause: e.Incident.Cause, OpenedAt: e.Incident.OpenedAt, ResolvedAt: e.Incident.ResolvedAt,
	})
	if err != nil {
		return err
	}
	return post(ctx, w.Client, w.URL, body, "webhook")
}

func post(ctx context.Context, client *http.Client, url string, body []byte, channel string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", channel, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// *url.Error embeds the full URL, which for Telegram contains the bot token; keep only the cause
		var urlErr *neturl.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("%s: request failed: %w", channel, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: status %d: %s", channel, resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return nil
}
