package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type tgResp struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

func Init(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("token is required")
	}
	if _, err := call(token, "getMe", nil); err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}
	if _, err := call(token, "setWebhook", map[string]interface{}{"url": ""}); err != nil {
		return fmt.Errorf("clear webhook failed: %w", err)
	}
	commands := []map[string]string{
		{"command": "start", "description": "Show onboarding and usage"},
		{"command": "help", "description": "Show usage and examples"},
		{"command": "reset", "description": "Reset chat session"},
		{"command": "model", "description": "Show/set active model"},
		{"command": "tools", "description": "List available tools"},
		{"command": "show", "description": "Show/toggle tools or thinking"},
		{"command": "verbose", "description": "Show or set tool-call tracing"},
		{"command": "think", "description": "Show or set thinking mode"},
		{"command": "status", "description": "Show status and token usage"},
	}
	if _, err := call(token, "setMyCommands", map[string]interface{}{"commands": commands}); err != nil {
		return fmt.Errorf("set commands failed: %w", err)
	}
	_ = ctx
	return nil
}

func call(token, method string, payload interface{}) (json.RawMessage, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", token, method)
	body := []byte("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = b
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var tr tgResp
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, err
	}
	if !tr.OK {
		return nil, fmt.Errorf("telegram error: %s", tr.Description)
	}
	return tr.Result, nil
}
