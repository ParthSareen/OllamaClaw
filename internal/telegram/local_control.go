package telegram

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/go-telegram/bot"
)

const (
	localControlHealthPath        = "/local/health"
	localControlTurnPath          = "/local/turn"
	localControlAudioPath         = "/local/audio"
	localControlTokenHeader       = "X-OllamaClaw-Local-Token"
	localControlJSONMaxBytes      = 64 * 1024
	localControlAudioMaxBytes     = telegramVoiceMaxBytes
	localControlMultipartOverage  = 1 * 1024 * 1024
	localControlMultipartMemory   = 1 * 1024 * 1024
	localControlSourceHotkeyText  = "hotkey_text"
	localControlSourceHotkeyVoice = "hotkey_voice"
)

type localTurnRequest struct {
	Text   string `json:"text"`
	Source string `json:"source"`
}

type localControlResponse struct {
	OK         bool   `json:"ok"`
	Status     string `json:"status,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	Source     string `json:"source,omitempty"`
}

func (r *Runner) startLocalControlServer(ctx context.Context, b *bot.Bot) (func(), error) {
	if !r.Cfg.LocalControl.Enabled {
		r.logf("local control server disabled")
		return func() {}, nil
	}
	if b == nil {
		return nil, fmt.Errorf("local control server requires an active telegram bot client")
	}
	cfg := r.Cfg.LocalControl
	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		listenAddr = config.Default().LocalControl.ListenAddr
	}
	if err := validateLocalControlListenAddr(listenAddr); err != nil {
		return nil, err
	}
	tokenPath := strings.TrimSpace(cfg.TokenPath)
	if tokenPath == "" {
		tokenPath = config.Default().LocalControl.TokenPath
	}
	token, err := ensureLocalControlToken(tokenPath)
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           r.localControlHandler(token, b),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen local control server on %s: %w", listenAddr, err)
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		})
	}

	go func() {
		<-ctx.Done()
		stop()
	}()
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			r.logf("local control server stopped with error: %v", r.redactError(err))
		}
	}()

	r.logf("local control server listening: addr=%s actual_addr=%s token_path=%s", listenAddr, ln.Addr().String(), tokenPath)
	return stop, nil
}

func (r *Runner) localControlHandler(token string, b *bot.Bot) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(localControlHealthPath, func(w http.ResponseWriter, req *http.Request) {
		if !authorizeLocalControlRequest(w, req, token) {
			return
		}
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeLocalControlJSON(w, http.StatusOK, localControlResponse{OK: true, Status: "ready"})
	})
	mux.HandleFunc(localControlTurnPath, func(w http.ResponseWriter, req *http.Request) {
		if !authorizeLocalControlRequest(w, req, token) {
			return
		}
		r.handleLocalControlTurn(w, req, b)
	})
	mux.HandleFunc(localControlAudioPath, func(w http.ResponseWriter, req *http.Request) {
		if !authorizeLocalControlRequest(w, req, token) {
			return
		}
		r.handleLocalControlAudio(w, req, b)
	})
	return mux
}

func (r *Runner) handleLocalControlTurn(w http.ResponseWriter, req *http.Request, b *bot.Bot) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, localControlJSONMaxBytes)
	var payload localTurnRequest
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(payload.Text)
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	source := localControlSourceOrDefault(payload.Source, localControlSourceHotkeyText)
	queued, err := r.queueLocalControlTurn(b, pendingTurn{
		text:        text,
		localSource: source,
		noDebounce:  true,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeLocalControlJSON(w, http.StatusAccepted, localControlResponse{OK: true, Status: "queued", Generation: queued.generation, Source: source})
}

func (r *Runner) handleLocalControlAudio(w http.ResponseWriter, req *http.Request, b *bot.Bot) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, localControlAudioMaxBytes+localControlMultipartOverage)
	if err := req.ParseMultipartForm(localControlMultipartMemory); err != nil {
		http.Error(w, "invalid multipart payload", http.StatusBadRequest)
		return
	}
	audioPath, cleanup, err := saveLocalControlAudio(req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errLocalControlAudioTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	source := localControlSourceOrDefault(req.FormValue("source"), localControlSourceHotkeyVoice)
	queued, err := r.queueLocalControlTurn(b, pendingTurn{
		localSource:  source,
		localAudio:   audioPath,
		localCleanup: cleanup,
		noDebounce:   true,
	})
	if err != nil {
		cleanup()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeLocalControlJSON(w, http.StatusAccepted, localControlResponse{OK: true, Status: "queued", Generation: queued.generation, Source: source})
}

func (r *Runner) queueLocalControlTurn(b *bot.Bot, turn pendingTurn) (pendingTurn, error) {
	chatID := r.Cfg.Telegram.OwnerChatID
	userID := r.Cfg.Telegram.OwnerUserID
	if chatID == 0 || userID == 0 {
		return pendingTurn{}, fmt.Errorf("telegram owner allowlist is not configured")
	}
	sessionKey := strconv.FormatInt(chatID, 10)
	turn.chatID = chatID
	turn.userID = userID
	turn.sessionKey = sessionKey
	turn.bot = b
	if turn.messageCount <= 0 {
		turn.messageCount = 1
	}
	queued := r.enqueuePendingTurn(sessionKey, turn)
	r.logf("local control queued: chat=%d session_key=%s source=%s generation=%d ready_at=%s audio=%t chars=%d", chatID, sessionKey, strings.TrimSpace(queued.localSource), queued.generation, queued.readyAt.UTC().Format(time.RFC3339Nano), strings.TrimSpace(queued.localAudio) != "", len(queued.text))
	r.schedulePendingTurnDrain(sessionKey, queued.generation)
	return queued, nil
}

func ensureLocalControlToken(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("local control token path is required")
	}
	if b, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(b))
		if token != "" {
			_ = os.Chmod(path, 0o600)
			return token, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read local control token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create local control token dir: %w", err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate local control token: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write local control token: %w", err)
	}
	return token, nil
}

func validateLocalControlListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("invalid local control listen_addr %q: %w", addr, err)
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("local control listen_addr must bind to localhost or a loopback IP, got %q", addr)
	}
	return nil
}

func authorizeLocalControlRequest(w http.ResponseWriter, req *http.Request, token string) bool {
	got := strings.TrimSpace(req.Header.Get(localControlTokenHeader))
	if strings.TrimSpace(token) == "" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

var errLocalControlAudioTooLarge = errors.New("audio is too large")

func saveLocalControlAudio(req *http.Request) (string, func(), error) {
	file, header, err := req.FormFile("audio")
	if err != nil {
		return "", func() {}, fmt.Errorf("audio file is required")
	}
	defer file.Close()

	dir, err := os.MkdirTemp("", "ollamaclaw-local-audio-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create local audio temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(header.Filename)))
	if ext == "" || len(ext) > 12 {
		ext = ".wav"
	}
	audioPath := filepath.Join(dir, "input"+ext)
	out, err := os.OpenFile(audioPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("create local audio file: %w", err)
	}
	limited := &io.LimitedReader{R: file, N: localControlAudioMaxBytes + 1}
	n, copyErr := io.Copy(out, limited)
	closeErr := out.Close()
	if copyErr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("save local audio: %w", copyErr)
	}
	if closeErr != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close local audio file: %w", closeErr)
	}
	if n == 0 {
		cleanup()
		return "", func() {}, fmt.Errorf("audio file is empty")
	}
	if n > localControlAudioMaxBytes {
		cleanup()
		return "", func() {}, errLocalControlAudioTooLarge
	}
	return audioPath, cleanup, nil
}

func localControlSourceOrDefault(raw, fallback string) string {
	source := strings.TrimSpace(raw)
	if source == "" {
		source = fallback
	}
	if len(source) > 80 {
		source = source[:80]
	}
	return source
}

func writeLocalControlJSON(w http.ResponseWriter, status int, payload localControlResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
