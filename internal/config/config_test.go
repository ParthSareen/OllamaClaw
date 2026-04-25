package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCreatesDefaultConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.DefaultModel != "kimi-k2.5:cloud" {
		t.Fatalf("unexpected model: %s", cfg.DefaultModel)
	}
	if strings.TrimSpace(cfg.GitHubWebhook.ListenAddr) != "127.0.0.1:8787" {
		t.Fatalf("unexpected github webhook listen addr: %q", cfg.GitHubWebhook.ListenAddr)
	}
	if cfg.Voice.TranscriptionModel != "gemma4:e2b" {
		t.Fatalf("unexpected voice transcription model: %q", cfg.Voice.TranscriptionModel)
	}
	if !strings.HasSuffix(cfg.Voice.KokoroPython, filepath.Join(".ollamaclaw", "kokoro-test", "venv", "bin", "python")) {
		t.Fatalf("unexpected kokoro python path: %q", cfg.Voice.KokoroPython)
	}
	path, _ := ConfigPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to exist: %v", err)
	}
}

func TestRedactedMasksToken(t *testing.T) {
	cfg := Default()
	cfg.Telegram.BotToken = "secret"
	cfg.GitHubWebhook.Secret = "webhook-secret"
	red := Redacted(cfg)
	if red.Telegram.BotToken != "***" {
		t.Fatalf("expected redacted token, got %q", red.Telegram.BotToken)
	}
	if red.GitHubWebhook.Secret != "***" {
		t.Fatalf("expected redacted github webhook secret, got %q", red.GitHubWebhook.Secret)
	}
}

func TestSaveExpandsDBPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Default()
	cfg.DBPath = "~/custom/state.db"
	cfg.LogPath = "~/custom/ollamaclaw.log"
	cfg.Voice.KokoroPython = "~/custom/kokoro/bin/python"
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	expected := filepath.Join(home, "custom", "state.db")
	if loaded.DBPath != expected {
		t.Fatalf("expected %s, got %s", expected, loaded.DBPath)
	}
	expectedLog := filepath.Join(home, "custom", "ollamaclaw.log")
	if loaded.LogPath != expectedLog {
		t.Fatalf("expected %s, got %s", expectedLog, loaded.LogPath)
	}
	expectedKokoro := filepath.Join(home, "custom", "kokoro", "bin", "python")
	if loaded.Voice.KokoroPython != expectedKokoro {
		t.Fatalf("expected %s, got %s", expectedKokoro, loaded.Voice.KokoroPython)
	}
}

func TestSystemPromptOverlayPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	overlayPath, err := SystemPromptOverlayPath()
	if err != nil {
		t.Fatalf("SystemPromptOverlayPath() error: %v", err)
	}
	historyPath, err := SystemPromptOverlayHistoryPath()
	if err != nil {
		t.Fatalf("SystemPromptOverlayHistoryPath() error: %v", err)
	}
	if !strings.HasSuffix(overlayPath, filepath.Join(".ollamaclaw", "system_prompt.overlay.md")) {
		t.Fatalf("unexpected overlay path: %s", overlayPath)
	}
	if !strings.HasSuffix(historyPath, filepath.Join(".ollamaclaw", "system_prompt.overlay.history.jsonl")) {
		t.Fatalf("unexpected history path: %s", historyPath)
	}
}

func TestLoadNormalizesGitHubWebhookAllowlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Default()
	cfg.GitHubWebhook.Secret = "secret"
	cfg.GitHubWebhook.OwnerLogin = "parth"
	cfg.GitHubWebhook.Enabled = true
	cfg.GitHubWebhook.RepoAllowlist = []string{"ollama/ollama", " ollama/ollama ", "Ollama/Ollama", "", "openai/openai"}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !loaded.GitHubWebhook.Enabled {
		t.Fatalf("expected github webhook to stay enabled with valid creds")
	}
	if got := loaded.GitHubWebhook.RepoAllowlist; len(got) != 2 {
		t.Fatalf("expected deduped allowlist length 2, got %d (%v)", len(got), got)
	}
}
