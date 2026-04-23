package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultConfigDirName = ".ollamaclaw"
	defaultConfigFile    = "config.json"
	defaultStateDBFile   = "state.db"
	defaultLogFile       = "ollamaclaw.log"
	defaultGitHubWebhook = "127.0.0.1:8787"
	defaultPromptFile    = "system_prompt.txt"
	defaultPromptOverlay = "system_prompt.overlay.md"
	defaultPromptHistory = "system_prompt.overlay.history.jsonl"
	defaultCoreMemFile   = "core_memories.md"
)

// Config stores runtime settings for OllamaClaw.
type Config struct {
	OllamaHost          string         `json:"ollama_host"`
	DefaultModel        string         `json:"default_model"`
	DBPath              string         `json:"db_path"`
	LogPath             string         `json:"log_path"`
	CompactionThreshold float64        `json:"compaction_threshold"`
	KeepRecentTurns     int            `json:"keep_recent_turns"`
	ContextWindowTokens int            `json:"context_window_tokens"`
	ToolOutputMaxBytes  int            `json:"tool_output_max_bytes"`
	BashTimeoutSeconds  int            `json:"bash_timeout_seconds"`
	Telegram            TelegramConfig `json:"telegram"`
	GitHubWebhook       GitHubWebhook  `json:"github_webhook"`
}

type TelegramConfig struct {
	BotToken    string `json:"bot_token"`
	OwnerChatID int64  `json:"owner_chat_id"`
	OwnerUserID int64  `json:"owner_user_id"`
}

type GitHubWebhook struct {
	Enabled       bool     `json:"enabled"`
	ListenAddr    string   `json:"listen_addr"`
	Secret        string   `json:"secret"`
	OwnerLogin    string   `json:"owner_login"`
	RepoAllowlist []string `json:"repo_allowlist"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, defaultConfigDirName)
	return Config{
		OllamaHost:          "http://localhost:11434",
		DefaultModel:        "kimi-k2.5:cloud",
		DBPath:              filepath.Join(base, defaultStateDBFile),
		LogPath:             filepath.Join(base, defaultLogFile),
		CompactionThreshold: 0.8,
		KeepRecentTurns:     8,
		ContextWindowTokens: 252000,
		ToolOutputMaxBytes:  16 * 1024,
		BashTimeoutSeconds:  120,
		Telegram:            TelegramConfig{},
		GitHubWebhook: GitHubWebhook{
			Enabled:       false,
			ListenAddr:    defaultGitHubWebhook,
			Secret:        "",
			OwnerLogin:    "",
			RepoAllowlist: nil,
		},
	}
}

func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, defaultConfigDirName), nil
}

func ConfigPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, defaultConfigFile), nil
}

func SystemPromptPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, defaultPromptFile), nil
}

func SystemPromptOverlayPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, defaultPromptOverlay), nil
}

func SystemPromptOverlayHistoryPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, defaultPromptHistory), nil
}

func CoreMemoriesPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, defaultCoreMemFile), nil
}

func EnsureBaseDir() error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

func expandPath(p string) (string, error) {
	if p == "" {
		return p, nil
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~/")), nil
	}
	return p, nil
}

func sanitize(cfg *Config) {
	defaults := Default()
	if cfg.OllamaHost == "" {
		cfg.OllamaHost = defaults.OllamaHost
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = defaults.DefaultModel
	}
	if cfg.DBPath == "" {
		cfg.DBPath = defaults.DBPath
	}
	if cfg.LogPath == "" {
		cfg.LogPath = defaults.LogPath
	}
	if cfg.CompactionThreshold <= 0 || cfg.CompactionThreshold > 1 {
		cfg.CompactionThreshold = defaults.CompactionThreshold
	}
	if cfg.KeepRecentTurns <= 0 {
		cfg.KeepRecentTurns = defaults.KeepRecentTurns
	}
	if cfg.ContextWindowTokens <= 0 {
		cfg.ContextWindowTokens = defaults.ContextWindowTokens
	}
	if cfg.ToolOutputMaxBytes <= 0 {
		cfg.ToolOutputMaxBytes = defaults.ToolOutputMaxBytes
	}
	if cfg.BashTimeoutSeconds <= 0 {
		cfg.BashTimeoutSeconds = defaults.BashTimeoutSeconds
	}
	cfg.GitHubWebhook.ListenAddr = strings.TrimSpace(cfg.GitHubWebhook.ListenAddr)
	if cfg.GitHubWebhook.ListenAddr == "" {
		cfg.GitHubWebhook.ListenAddr = defaults.GitHubWebhook.ListenAddr
	}
	cfg.GitHubWebhook.Secret = strings.TrimSpace(cfg.GitHubWebhook.Secret)
	cfg.GitHubWebhook.OwnerLogin = strings.TrimSpace(cfg.GitHubWebhook.OwnerLogin)
	cfg.GitHubWebhook.RepoAllowlist = normalizeRepoAllowlist(cfg.GitHubWebhook.RepoAllowlist)
	if cfg.GitHubWebhook.Secret == "" || cfg.GitHubWebhook.OwnerLogin == "" {
		cfg.GitHubWebhook.Enabled = false
	}
}

func Load() (Config, error) {
	if err := EnsureBaseDir(); err != nil {
		return Config{}, err
	}
	path, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}
	defaults := Default()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := Save(defaults); err != nil {
			return Config{}, err
		}
		return defaults, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	cfg := defaults
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	sanitize(&cfg)
	cfg.DBPath, err = expandPath(cfg.DBPath)
	if err != nil {
		return Config{}, fmt.Errorf("expand db path: %w", err)
	}
	cfg.LogPath, err = expandPath(cfg.LogPath)
	if err != nil {
		return Config{}, fmt.Errorf("expand log path: %w", err)
	}
	return cfg, nil
}

func Save(cfg Config) error {
	if err := EnsureBaseDir(); err != nil {
		return err
	}
	sanitize(&cfg)
	cfg.DBPath, _ = expandPath(cfg.DBPath)
	cfg.LogPath, _ = expandPath(cfg.LogPath)
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func Redacted(cfg Config) Config {
	out := cfg
	if out.Telegram.BotToken != "" {
		out.Telegram.BotToken = "***"
	}
	if out.GitHubWebhook.Secret != "" {
		out.GitHubWebhook.Secret = "***"
	}
	return out
}

func normalizeRepoAllowlist(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		repo := strings.TrimSpace(item)
		if repo == "" {
			continue
		}
		repoKey := strings.ToLower(repo)
		if _, ok := seen[repoKey]; ok {
			continue
		}
		seen[repoKey] = struct{}{}
		out = append(out, repo)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
