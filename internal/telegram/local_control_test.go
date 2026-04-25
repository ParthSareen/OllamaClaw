package telegram

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/go-telegram/bot"
)

func TestEnsureLocalControlTokenCreatesAndLoadsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local_control.token")
	token, err := ensureLocalControlToken(path)
	if err != nil {
		t.Fatalf("ensureLocalControlToken() error: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("expected 64-char token, got %d", len(token))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected token file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected token file mode 0600, got %o", got)
	}
	loaded, err := ensureLocalControlToken(path)
	if err != nil {
		t.Fatalf("ensureLocalControlToken() load error: %v", err)
	}
	if loaded != token {
		t.Fatalf("expected token reload to match")
	}
}

func TestValidateLocalControlListenAddrRequiresLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8790", "localhost:8790", "[::1]:8790"} {
		if err := validateLocalControlListenAddr(addr); err != nil {
			t.Fatalf("expected %s to validate: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8790", ":8790", "192.168.1.2:8790"} {
		if err := validateLocalControlListenAddr(addr); err == nil {
			t.Fatalf("expected %s to be rejected", addr)
		}
	}
}

func TestLocalControlAuthAndHealth(t *testing.T) {
	handler := (&Runner{}).localControlHandler("secret", nil)

	req := httptest.NewRequest(http.MethodGet, localControlHealthPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected missing token 401, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, localControlHealthPath, nil)
	req.Header.Set(localControlTokenHeader, "bad")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected bad token 401, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, localControlHealthPath, nil)
	req.Header.Set(localControlTokenHeader, "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected health 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("expected ok health response, got %s", rec.Body.String())
	}
}

func TestLocalControlTurnValidationAndQueue(t *testing.T) {
	cfg := config.Default()
	cfg.Telegram.OwnerChatID = 123
	cfg.Telegram.OwnerUserID = 456
	r := &Runner{Cfg: cfg}
	executed := make(chan string, 1)
	r.turnExecutor = func(ctx context.Context, turnCtx context.Context, b *bot.Bot, chatID, userID int64, sessionKey, text string, imageFileIDs []string) {
		executed <- text
	}
	handler := r.localControlHandler("secret", nil)

	req := httptest.NewRequest(http.MethodPost, localControlTurnPath, strings.NewReader(`{"text":"   "}`))
	req.Header.Set(localControlTokenHeader, "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected empty text 400, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, localControlTurnPath, strings.NewReader(`{"text":"hello from hotkey","source":"hotkey_text"}`))
	req.Header.Set(localControlTokenHeader, "secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected accepted turn, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-executed:
		if got != "hello from hotkey" {
			t.Fatalf("expected queued text, got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for queued local turn")
	}
}

func TestLocalControlAudioValidationAndQueue(t *testing.T) {
	cfg := config.Default()
	cfg.Telegram.OwnerChatID = 123
	cfg.Telegram.OwnerUserID = 456
	r := &Runner{Cfg: cfg}
	executed := make(chan struct{}, 1)
	r.turnExecutor = func(ctx context.Context, turnCtx context.Context, b *bot.Bot, chatID, userID int64, sessionKey, text string, imageFileIDs []string) {
		executed <- struct{}{}
	}
	handler := r.localControlHandler("secret", nil)

	req := httptest.NewRequest(http.MethodPost, localControlAudioPath, strings.NewReader(""))
	req.Header.Set(localControlTokenHeader, "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected missing audio 400, got %d", rec.Code)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("audio", "clip.wav")
	if err != nil {
		t.Fatalf("CreateFormFile() error: %v", err)
	}
	if _, err := part.Write([]byte("fake-wav-bytes")); err != nil {
		t.Fatalf("write multipart audio: %v", err)
	}
	if err := writer.WriteField("source", "hotkey_voice"); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, localControlAudioPath, &body)
	req.Header.Set(localControlTokenHeader, "secret")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected accepted audio, got %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-executed:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for queued local audio turn")
	}
}

func TestVoiceOutputModeParsingAndPersistence(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "mac", want: "mac", ok: true},
		{in: "local", want: "mac", ok: true},
		{in: "telegram", want: "telegram", ok: true},
		{in: "tg", want: "telegram", ok: true},
		{in: "both", want: "both", ok: true},
		{in: "maybe", want: "", ok: false},
	}
	for _, tc := range tests {
		got, ok := normalizeVoiceOutputMode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("normalizeVoiceOutputMode(%q) = (%q,%t), want (%q,%t)", tc.in, got, ok, tc.want, tc.ok)
		}
	}

	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	r := &Runner{Store: store}
	mode, err := r.sessionVoiceOutputMode(context.Background(), "123")
	if err != nil {
		t.Fatalf("default voice output mode error: %v", err)
	}
	if mode != "mac" {
		t.Fatalf("expected default mac output, got %s", mode)
	}
	if err := r.setSessionVoiceOutputMode(context.Background(), "123", "telegram"); err != nil {
		t.Fatalf("set voice output mode: %v", err)
	}
	mode, err = r.sessionVoiceOutputMode(context.Background(), "123")
	if err != nil {
		t.Fatalf("read voice output mode: %v", err)
	}
	if mode != "telegram" {
		t.Fatalf("expected telegram output, got %s", mode)
	}
}

func TestResponseDeliveryDistinguishesTurnSources(t *testing.T) {
	r := &Runner{}

	telegramText := r.responseDeliveryForTurn(context.Background(), "123", turnExecutionOptions{})
	if !telegramText.SendText || telegramText.TelegramVoice || telegramText.LocalSpeech {
		t.Fatalf("telegram text delivery = %+v, want text only", telegramText)
	}

	telegramVoice := r.responseDeliveryForTurn(context.Background(), "123", turnExecutionOptions{TelegramVoiceInput: true})
	if !telegramVoice.SendText || !telegramVoice.TelegramVoice || telegramVoice.LocalSpeech {
		t.Fatalf("telegram voice delivery = %+v, want text + telegram voice by default", telegramVoice)
	}

	localText := r.responseDeliveryForTurn(context.Background(), "123", turnExecutionOptions{LocalSource: localControlSourceHotkeyText})
	if !localText.SendText || localText.TelegramVoice || localText.LocalSpeech {
		t.Fatalf("local text delivery = %+v, want text only", localText)
	}

	localVoice := r.responseDeliveryForTurn(context.Background(), "123", turnExecutionOptions{LocalSource: localControlSourceHotkeyVoice, LocalAudio: "/tmp/input.wav"})
	if !localVoice.SendText || localVoice.TelegramVoice || !localVoice.LocalSpeech {
		t.Fatalf("local voice delivery = %+v, want text + local speech by default", localVoice)
	}
}
