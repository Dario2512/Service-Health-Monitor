package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
)

// LogChannel writes alerts to the service log, which is what `docker logs`
// shows.
type LogChannel struct{ Log *slog.Logger }

func (LogChannel) Name() string { return "log" }

func (c LogChannel) Send(ctx context.Context, m Message) error {
	level := slog.LevelInfo
	if m.Kind == "down" {
		level = slog.LevelWarn
	}
	c.Log.Log(ctx, level, "ALERT "+m.Subject, "text", m.Text)
	return nil
}

// FileChannel appends each alert as one line of JSON, for anything that tails
// a file.
type FileChannel struct {
	Path string
	mu   sync.Mutex
}

func (c *FileChannel) Name() string { return "file" }

func (c *FileChannel) Send(_ context.Context, m Message) error {
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.OpenFile(c.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// WebhookChannel posts each alert as JSON. The body carries a top-level
// "text" field, which Slack, Mattermost and Discord-compatible incoming
// webhooks display as-is.
type WebhookChannel struct {
	URL    string
	Client *http.Client
}

func (c *WebhookChannel) Name() string { return "webhook" }

func (c *WebhookChannel) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(struct {
		Text string `json:"text"`
		Message
	}{Text: m.Subject + "\n" + m.Text, Message: m})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}
