package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type tgResp struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

var (
	telegramAPIBaseURL     = "https://api.telegram.org"
	telegramAPICallTimeout = 10 * time.Second
)

type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string {
	return e.msg
}

func (e redactedError) Unwrap() error {
	return e.err
}

func Init(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("token is required")
	}
	if _, err := call(ctx, token, "getMe", nil); err != nil {
		return fmt.Errorf("token validation failed: %w", redactTelegramError(token, err))
	}
	if _, err := call(ctx, token, "setWebhook", map[string]interface{}{"url": ""}); err != nil {
		return fmt.Errorf("clear webhook failed: %w", redactTelegramError(token, err))
	}
	if err := SyncCommands(ctx, token); err != nil {
		return fmt.Errorf("set commands failed: %w", err)
	}
	return nil
}

func SyncCommands(ctx context.Context, token string) error {
	commands := botCommandDefinitions()
	if _, err := call(ctx, token, "setMyCommands", map[string]interface{}{
		"commands": commands,
	}); err != nil {
		return redactTelegramError(token, err)
	}
	// Mirror command list for private chats explicitly so Telegram clients reliably refresh DM command menus.
	if _, err := call(ctx, token, "setMyCommands", map[string]interface{}{
		"scope":    map[string]string{"type": "all_private_chats"},
		"commands": commands,
	}); err != nil {
		return redactTelegramError(token, err)
	}
	return nil
}

func botCommandDefinitions() []map[string]string {
	return []map[string]string{
		{"command": "start", "description": "Show onboarding and usage"},
		{"command": "help", "description": "Show usage and examples"},
		{"command": "reset", "description": "Reset chat session"},
		{"command": "model", "description": "Show/set active model"},
		{"command": "tools", "description": "List available tools"},
		{"command": "reminder", "description": "List reminders, safety, and prefetch"},
		{"command": "agents", "description": "List, show, or cancel background Codex tasks"},
		{"command": "show", "description": "Show/toggle tools, thinking, or dreaming"},
		{"command": "verbose", "description": "Show or set tool/thinking traces"},
		{"command": "think", "description": "Show or set think value"},
		{"command": "voice", "description": "Show or set voice replies and output"},
		{"command": "dream", "description": "Trigger a core-memory refresh now"},
		{"command": "status", "description": "Show status and token usage"},
		{"command": "fullsystem", "description": "Show full system context"},
		{"command": "stop", "description": "Stop the active turn"},
		{"command": "restart", "description": "Restart the launch loop"},
	}
}

func call(ctx context.Context, token, method string, payload interface{}) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, telegramAPICallTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/bot%s/%s", telegramAPIBaseURL, token, method)
	body := []byte("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, redactTelegramError(token, fmt.Errorf("marshal telegram payload for %s: %w", method, err))
		}
		body = b
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, redactTelegramError(token, fmt.Errorf("create telegram request for %s: %w", method, err))
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, redactTelegramError(token, fmt.Errorf("do telegram request for %s: %w", method, err))
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, redactTelegramError(token, fmt.Errorf("read telegram response for %s: %w", method, err))
	}
	if res.StatusCode >= 300 {
		return nil, redactTelegramError(token, fmt.Errorf("status %d: %s", res.StatusCode, strings.TrimSpace(string(b))))
	}
	var tr tgResp
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, redactTelegramError(token, fmt.Errorf("decode telegram response for %s: %w", method, err))
	}
	if !tr.OK {
		return nil, redactTelegramError(token, fmt.Errorf("telegram error: %s", tr.Description))
	}
	return tr.Result, nil
}

func redactTelegramError(token string, err error) error {
	if err == nil || strings.TrimSpace(token) == "" {
		return err
	}
	msg := redactTelegramToken(token, err.Error())
	if msg == err.Error() {
		return err
	}
	return redactedError{
		msg: msg,
		err: err,
	}
}

func redactTelegramToken(token, s string) string {
	if strings.TrimSpace(token) == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}
