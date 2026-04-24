package telegram

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/go-telegram/bot"
)

const (
	githubWebhookPath               = "/webhooks/github"
	githubWebhookDeliverySettingKey = "github_webhook_delivery"
	githubWebhookMaxBodyBytes       = 1 << 20
)

type githubWebhookPayload struct {
	Action string `json:"action"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Draft   bool   `json:"draft"`
		Merged  bool   `json:"merged"`
	} `json:"pull_request"`
	Review *struct {
		State   string `json:"state"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	} `json:"review"`
	Comment *struct {
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	} `json:"comment"`
	CheckRun *struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	} `json:"check_run"`
	CheckSuite *struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
	} `json:"check_suite"`
}

func (r *Runner) startGitHubWebhookServer(ctx context.Context, b *bot.Bot) (func(), error) {
	if !githubWebhookEnabled(r.Cfg) {
		r.logf("github webhook server disabled")
		return func() {}, nil
	}
	if b == nil {
		return nil, fmt.Errorf("github webhook server requires an active telegram bot client")
	}
	cfg := r.Cfg.GitHubWebhook
	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		listenAddr = config.Default().GitHubWebhook.ListenAddr
	}
	if strings.TrimSpace(cfg.Secret) == "" || strings.TrimSpace(cfg.OwnerLogin) == "" {
		return nil, fmt.Errorf("github webhook config is incomplete (secret and owner_login are required)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc(githubWebhookPath, func(w http.ResponseWriter, req *http.Request) {
		r.handleGitHubWebhook(w, req, b)
	})
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen github webhook server on %s: %w", listenAddr, err)
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
			r.logf("github webhook server stopped with error: %v", r.redactError(err))
		}
	}()

	allowlist := "-"
	if len(cfg.RepoAllowlist) > 0 {
		allowlist = strings.Join(cfg.RepoAllowlist, ",")
	}
	r.logf("github webhook server listening: addr=%s path=%s owner_login=%s repo_allowlist=%s", listenAddr, githubWebhookPath, cfg.OwnerLogin, allowlist)
	return stop, nil
}

func (r *Runner) handleGitHubWebhook(w http.ResponseWriter, req *http.Request, b *bot.Bot) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deliveryID := strings.TrimSpace(req.Header.Get("X-GitHub-Delivery"))
	eventType := strings.TrimSpace(req.Header.Get("X-GitHub-Event"))
	if deliveryID == "" || eventType == "" {
		http.Error(w, "missing GitHub delivery headers", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, githubWebhookMaxBodyBytes+1))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if len(body) > githubWebhookMaxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifyGitHubWebhookSignature(strings.TrimSpace(r.Cfg.GitHubWebhook.Secret), body, req.Header.Get("X-Hub-Signature-256")) {
		r.logf("github webhook rejected: delivery=%s event=%s reason=invalid_signature", deliveryID, eventType)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	if strings.EqualFold(eventType, "ping") {
		r.logf("github webhook ping received: delivery=%s", deliveryID)
		writeWebhookResponse(w, http.StatusOK, "pong")
		return
	}

	var payload githubWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON payload", http.StatusBadRequest)
		return
	}

	accepted, reason := r.shouldAcceptGitHubWebhookPayload(eventType, payload)
	if !accepted {
		r.logf("github webhook ignored: delivery=%s event=%s action=%s reason=%s", deliveryID, eventType, strings.TrimSpace(payload.Action), reason)
		writeWebhookResponse(w, http.StatusOK, "ignored")
		return
	}

	seen, err := r.markGitHubWebhookDeliverySeen(req.Context(), deliveryID)
	if err != nil {
		r.logf("github webhook dedupe check failed: delivery=%s error=%v", deliveryID, r.redactError(err))
		http.Error(w, "internal dedupe error", http.StatusInternalServerError)
		return
	}
	if seen {
		r.logf("github webhook duplicate delivery ignored: delivery=%s event=%s", deliveryID, eventType)
		writeWebhookResponse(w, http.StatusOK, "duplicate")
		return
	}

	turnText := buildGitHubWebhookTurnText(eventType, deliveryID, payload, time.Now().UTC())
	if strings.TrimSpace(turnText) == "" {
		writeWebhookResponse(w, http.StatusOK, "ignored")
		return
	}
	if err := r.queueGitHubWebhookTurn(b, deliveryID, eventType, payload, turnText); err != nil {
		r.logf("github webhook queue failed: delivery=%s error=%v", deliveryID, r.redactError(err))
		http.Error(w, "failed to queue turn", http.StatusInternalServerError)
		return
	}
	writeWebhookResponse(w, http.StatusAccepted, "queued")
}

func (r *Runner) shouldAcceptGitHubWebhookPayload(eventType string, payload githubWebhookPayload) (bool, string) {
	action := strings.TrimSpace(payload.Action)
	if !shouldHandleGitHubAction(eventType, action) {
		return false, "unsupported event/action"
	}
	repo := strings.TrimSpace(payload.Repository.FullName)
	if repo == "" {
		return false, "missing repository full_name"
	}
	if !repoAllowed(repo, r.Cfg.GitHubWebhook.RepoAllowlist) {
		return false, "repository not in allowlist"
	}
	if !isGitHubCIEvent(eventType) {
		sender := strings.TrimSpace(payload.Sender.Login)
		if sender == "" {
			return false, "missing sender login"
		}
		owner := strings.TrimSpace(r.Cfg.GitHubWebhook.OwnerLogin)
		if owner != "" && !strings.EqualFold(owner, sender) {
			return false, "sender not configured owner login"
		}
	}
	return true, ""
}

func (r *Runner) queueGitHubWebhookTurn(b *bot.Bot, deliveryID, eventType string, payload githubWebhookPayload, turnText string) error {
	chatID := r.Cfg.Telegram.OwnerChatID
	userID := r.Cfg.Telegram.OwnerUserID
	if chatID == 0 || userID == 0 {
		return fmt.Errorf("telegram owner allowlist is not configured")
	}
	sessionKey := strconv.FormatInt(chatID, 10)
	queued := r.enqueuePendingTurn(sessionKey, pendingTurn{
		chatID:       chatID,
		userID:       userID,
		sessionKey:   sessionKey,
		text:         turnText,
		messageCount: 1,
		bot:          b,
	})
	r.logf("github webhook queued: delivery=%s event=%s action=%s repo=%s sender=%s generation=%d ready_at=%s", deliveryID, eventType, strings.TrimSpace(payload.Action), strings.TrimSpace(payload.Repository.FullName), strings.TrimSpace(payload.Sender.Login), queued.generation, queued.readyAt.UTC().Format(time.RFC3339Nano))
	r.schedulePendingTurnDrain(sessionKey, queued.generation)
	return nil
}

func (r *Runner) markGitHubWebhookDeliverySeen(ctx context.Context, deliveryID string) (bool, error) {
	if r.Store == nil {
		return false, fmt.Errorf("settings store unavailable")
	}
	key := githubWebhookDeliveryKey(deliveryID)
	if key == "" {
		return false, fmt.Errorf("delivery id is required")
	}
	r.webhookMu.Lock()
	defer r.webhookMu.Unlock()

	_, ok, err := r.Store.GetSetting(ctx, key)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	if err := r.Store.SetSetting(ctx, key, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return false, err
	}
	return false, nil
}

func githubWebhookDeliveryKey(deliveryID string) string {
	id := strings.TrimSpace(deliveryID)
	if id == "" {
		return ""
	}
	return githubWebhookDeliverySettingKey + ":" + id
}

func githubWebhookEnabled(cfg config.Config) bool {
	if !cfg.GitHubWebhook.Enabled {
		return false
	}
	return strings.TrimSpace(cfg.GitHubWebhook.Secret) != "" && strings.TrimSpace(cfg.GitHubWebhook.OwnerLogin) != ""
}

func verifyGitHubWebhookSignature(secret string, body []byte, signatureHeader string) bool {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return false
	}
	parts := strings.SplitN(strings.TrimSpace(signatureHeader), "=", 2)
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "sha256") {
		return false
	}
	signatureHex := strings.TrimSpace(parts[1])
	received, err := hex.DecodeString(signatureHex)
	if err != nil || len(received) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(received, expected)
}

func shouldHandleGitHubAction(eventType, action string) bool {
	eventType = strings.TrimSpace(strings.ToLower(eventType))
	action = strings.TrimSpace(strings.ToLower(action))
	switch eventType {
	case "pull_request":
		switch action {
		case "opened", "synchronize", "reopened", "ready_for_review":
			return true
		default:
			return false
		}
	case "pull_request_review":
		switch action {
		case "submitted", "edited", "dismissed":
			return true
		default:
			return false
		}
	case "pull_request_review_comment":
		switch action {
		case "created", "edited":
			return true
		default:
			return false
		}
	case "check_run", "check_suite":
		return action == "completed"
	default:
		return false
	}
}

func isGitHubCIEvent(eventType string) bool {
	switch strings.TrimSpace(strings.ToLower(eventType)) {
	case "check_run", "check_suite":
		return true
	default:
		return false
	}
}

func repoAllowed(repo string, allowlist []string) bool {
	repo = strings.TrimSpace(strings.ToLower(repo))
	if repo == "" {
		return false
	}
	if len(allowlist) == 0 {
		return true
	}
	for _, item := range allowlist {
		if repo == strings.TrimSpace(strings.ToLower(item)) {
			return true
		}
	}
	return false
}

func buildGitHubWebhookTurnText(eventType, deliveryID string, payload githubWebhookPayload, receivedAt time.Time) string {
	repo := strings.TrimSpace(payload.Repository.FullName)
	sender := strings.TrimSpace(payload.Sender.Login)
	action := strings.TrimSpace(payload.Action)

	prNumber := 0
	prTitle := ""
	prURL := ""
	if payload.PullRequest != nil {
		prNumber = payload.PullRequest.Number
		prTitle = strings.TrimSpace(payload.PullRequest.Title)
		prURL = strings.TrimSpace(payload.PullRequest.HTMLURL)
	}

	lines := []string{
		"GitHub webhook trigger (proactive run):",
		fmt.Sprintf("delivery_id: %s", deliveryID),
		fmt.Sprintf("received_at_utc: %s", receivedAt.UTC().Format(time.RFC3339)),
		fmt.Sprintf("event: %s", strings.TrimSpace(eventType)),
		fmt.Sprintf("action: %s", fallbackText(action, "-")),
		fmt.Sprintf("repository: %s", fallbackText(repo, "-")),
		fmt.Sprintf("sender: %s", fallbackText(sender, "-")),
	}
	if prNumber > 0 {
		lines = append(lines, fmt.Sprintf("pull_request_number: %d", prNumber))
	}
	if prTitle != "" {
		lines = append(lines, fmt.Sprintf("pull_request_title: %s", prTitle))
	}
	if prURL != "" {
		lines = append(lines, fmt.Sprintf("pull_request_url: %s", prURL))
	}
	if payload.CheckRun != nil {
		lines = append(lines, fmt.Sprintf("check_run: %s", fallbackText(strings.TrimSpace(payload.CheckRun.Name), "-")))
		lines = append(lines, fmt.Sprintf("check_run_status: %s", fallbackText(strings.TrimSpace(payload.CheckRun.Status), "-")))
		lines = append(lines, fmt.Sprintf("check_run_conclusion: %s", fallbackText(strings.TrimSpace(payload.CheckRun.Conclusion), "-")))
		if u := strings.TrimSpace(payload.CheckRun.HTMLURL); u != "" {
			lines = append(lines, fmt.Sprintf("check_run_url: %s", u))
		}
	}
	if payload.CheckSuite != nil {
		lines = append(lines, fmt.Sprintf("check_suite_status: %s", fallbackText(strings.TrimSpace(payload.CheckSuite.Status), "-")))
		lines = append(lines, fmt.Sprintf("check_suite_conclusion: %s", fallbackText(strings.TrimSpace(payload.CheckSuite.Conclusion), "-")))
		if hb := strings.TrimSpace(payload.CheckSuite.HeadBranch); hb != "" {
			lines = append(lines, fmt.Sprintf("check_suite_head_branch: %s", hb))
		}
		if hs := strings.TrimSpace(payload.CheckSuite.HeadSHA); hs != "" {
			lines = append(lines, fmt.Sprintf("check_suite_head_sha: %s", hs))
		}
	}
	if payload.Review != nil {
		lines = append(lines, fmt.Sprintf("review_state: %s", fallbackText(strings.TrimSpace(payload.Review.State), "-")))
		if u := strings.TrimSpace(payload.Review.HTMLURL); u != "" {
			lines = append(lines, fmt.Sprintf("review_url: %s", u))
		}
	}
	if payload.Comment != nil {
		if u := strings.TrimSpace(payload.Comment.HTMLURL); u != "" {
			lines = append(lines, fmt.Sprintf("comment_url: %s", u))
		}
		comment := strings.TrimSpace(payload.Comment.Body)
		if comment != "" {
			lines = append(lines, fmt.Sprintf("comment_excerpt: %s", truncateWebhookField(comment, 280)))
		}
	}
	lines = append(lines, "")
	lines = append(lines, "Please proactively assess this GitHub update and send me a concise status plus next recommended actions.")
	lines = append(lines, "Use tools to fetch fresh state before concluding (for PR/CI status, run current GitHub queries).")
	return strings.Join(lines, "\n")
}

func fallbackText(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}

func truncateWebhookField(v string, max int) string {
	v = strings.Join(strings.Fields(strings.TrimSpace(v)), " ")
	if len(v) <= max {
		return v
	}
	if max <= 3 {
		return v[:max]
	}
	return v[:max-3] + "..."
}

func writeWebhookResponse(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": strings.TrimSpace(message)})
}
