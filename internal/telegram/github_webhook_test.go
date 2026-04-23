package telegram

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
)

func TestVerifyGitHubWebhookSignature(t *testing.T) {
	secret := "abc123"
	body := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyGitHubWebhookSignature(secret, body, signature) {
		t.Fatalf("expected signature to validate")
	}
	if verifyGitHubWebhookSignature(secret, body, "sha256=deadbeef") {
		t.Fatalf("expected invalid signature to fail")
	}
	if verifyGitHubWebhookSignature(secret, body, "") {
		t.Fatalf("expected empty signature header to fail")
	}
}

func TestShouldHandleGitHubAction(t *testing.T) {
	cases := []struct {
		event  string
		action string
		want   bool
	}{
		{event: "pull_request", action: "opened", want: true},
		{event: "pull_request", action: "closed", want: false},
		{event: "pull_request_review", action: "submitted", want: true},
		{event: "pull_request_review_comment", action: "created", want: true},
		{event: "check_run", action: "completed", want: true},
		{event: "check_suite", action: "requested", want: false},
	}
	for _, tc := range cases {
		if got := shouldHandleGitHubAction(tc.event, tc.action); got != tc.want {
			t.Fatalf("shouldHandleGitHubAction(%q,%q)=%t want=%t", tc.event, tc.action, got, tc.want)
		}
	}
}

func TestRepoAllowed(t *testing.T) {
	if !repoAllowed("ollama/ollama", nil) {
		t.Fatalf("expected repoAllowed with empty allowlist")
	}
	if !repoAllowed("Ollama/Ollama", []string{"ollama/ollama"}) {
		t.Fatalf("expected case-insensitive allowlist match")
	}
	if repoAllowed("openai/openai", []string{"ollama/ollama"}) {
		t.Fatalf("expected non-allowlisted repo to fail")
	}
}

func TestShouldAcceptGitHubWebhookPayload(t *testing.T) {
	cfg := config.Default()
	cfg.GitHubWebhook.OwnerLogin = "parth"
	cfg.GitHubWebhook.Secret = "secret"
	cfg.GitHubWebhook.Enabled = true
	cfg.GitHubWebhook.RepoAllowlist = []string{"ollama/ollama"}
	r := &Runner{Cfg: cfg}

	payload := githubWebhookPayload{
		Action: "opened",
	}
	payload.Sender.Login = "parth"
	payload.Repository.FullName = "ollama/ollama"

	ok, reason := r.shouldAcceptGitHubWebhookPayload("pull_request", payload)
	if !ok {
		t.Fatalf("expected payload to be accepted, reason=%s", reason)
	}

	payload.Sender.Login = "other"
	ok, _ = r.shouldAcceptGitHubWebhookPayload("pull_request", payload)
	if ok {
		t.Fatalf("expected sender mismatch to be rejected")
	}

	payload.Sender.Login = "parth"
	payload.Repository.FullName = "openai/openai"
	ok, _ = r.shouldAcceptGitHubWebhookPayload("pull_request", payload)
	if ok {
		t.Fatalf("expected repo mismatch to be rejected")
	}
}

func TestBuildGitHubWebhookTurnText(t *testing.T) {
	payload := githubWebhookPayload{
		Action: "synchronize",
	}
	payload.Sender.Login = "parth"
	payload.Repository.FullName = "ollama/ollama"
	payload.PullRequest = &struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Draft   bool   `json:"draft"`
		Merged  bool   `json:"merged"`
	}{
		Number:  15072,
		Title:   "launch: set default model",
		HTMLURL: "https://github.com/ollama/ollama/pull/15072",
	}
	payload.CheckRun = &struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HTMLURL    string `json:"html_url"`
	}{
		Name:       "build",
		Status:     "completed",
		Conclusion: "success",
		HTMLURL:    "https://github.com/ollama/ollama/actions/runs/123",
	}
	text := buildGitHubWebhookTurnText("pull_request", "delivery-1", payload, time.Date(2026, 4, 20, 14, 0, 0, 0, time.UTC))
	for _, want := range []string{
		"GitHub webhook trigger (proactive run):",
		"delivery_id: delivery-1",
		"event: pull_request",
		"repository: ollama/ollama",
		"pull_request_number: 15072",
		"check_run: build",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected turn text to include %q, got:\n%s", want, text)
		}
	}
}

func TestMarkGitHubWebhookDeliverySeen(t *testing.T) {
	store, err := db.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	r := &Runner{Store: store}
	seen, err := r.markGitHubWebhookDeliverySeen(context.Background(), "abc-123")
	if err != nil {
		t.Fatalf("first mark failed: %v", err)
	}
	if seen {
		t.Fatalf("expected first delivery mark to be unseen")
	}
	seen, err = r.markGitHubWebhookDeliverySeen(context.Background(), "abc-123")
	if err != nil {
		t.Fatalf("second mark failed: %v", err)
	}
	if !seen {
		t.Fatalf("expected second delivery mark to be seen")
	}
}
