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
	defaultLocalControl  = "127.0.0.1:8790"
	defaultLocalToken    = "local_control.token"
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
	Voice               VoiceConfig    `json:"voice"`
	GitHubWebhook       GitHubWebhook  `json:"github_webhook"`
	LocalControl        LocalControl   `json:"local_control"`
	Subagents           SubagentConfig `json:"subagents"`
}

type TelegramConfig struct {
	BotToken    string `json:"bot_token"`
	OwnerChatID int64  `json:"owner_chat_id"`
	OwnerUserID int64  `json:"owner_user_id"`
}

type VoiceConfig struct {
	TranscriptionModel string  `json:"transcription_model"`
	OllamaBinary       string  `json:"ollama_binary"`
	FFmpegBinary       string  `json:"ffmpeg_binary"`
	KokoroPython       string  `json:"kokoro_python"`
	KokoroVoice        string  `json:"kokoro_voice"`
	KokoroLangCode     string  `json:"kokoro_lang_code"`
	KokoroSpeed        float64 `json:"kokoro_speed"`
	MaxSpeechChars     int     `json:"max_speech_chars"`
}

type GitHubWebhook struct {
	Enabled       bool     `json:"enabled"`
	ListenAddr    string   `json:"listen_addr"`
	Secret        string   `json:"secret"`
	OwnerLogin    string   `json:"owner_login"`
	RepoAllowlist []string `json:"repo_allowlist"`
}

type LocalControl struct {
	Enabled    bool   `json:"enabled"`
	ListenAddr string `json:"listen_addr"`
	TokenPath  string `json:"token_path"`
}

type SubagentConfig struct {
	Enabled                bool   `json:"enabled"`
	CodexBinary            string `json:"codex_binary"`
	RootDir                string `json:"root_dir"`
	MaxConcurrent          int    `json:"max_concurrent"`
	DefaultTimeoutMinutes  int    `json:"default_timeout_minutes"`
	DefaultModel           string `json:"default_model"`
	DefaultProfile         string `json:"default_profile"`
	DefaultReasoningEffort string `json:"default_reasoning_effort"`
	Sandbox                string `json:"sandbox"`
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
		Voice: VoiceConfig{
			TranscriptionModel: "gemma4:e2b",
			OllamaBinary:       "ollama",
			FFmpegBinary:       "ffmpeg",
			KokoroPython:       filepath.Join(base, "kokoro-test", "venv", "bin", "python"),
			KokoroVoice:        "af_heart",
			KokoroLangCode:     "a",
			KokoroSpeed:        1.0,
			MaxSpeechChars:     1200,
		},
		GitHubWebhook: GitHubWebhook{
			Enabled:       false,
			ListenAddr:    defaultGitHubWebhook,
			Secret:        "",
			OwnerLogin:    "",
			RepoAllowlist: nil,
		},
		LocalControl: LocalControl{
			Enabled:    true,
			ListenAddr: defaultLocalControl,
			TokenPath:  filepath.Join(base, defaultLocalToken),
		},
		Subagents: SubagentConfig{
			Enabled:                true,
			CodexBinary:            "codex",
			RootDir:                filepath.Join(base, "subagents"),
			MaxConcurrent:          3,
			DefaultTimeoutMinutes:  45,
			DefaultModel:           "",
			DefaultProfile:         "",
			DefaultReasoningEffort: "xhigh",
			Sandbox:                "workspace-write",
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
	cfg.Voice.TranscriptionModel = strings.TrimSpace(cfg.Voice.TranscriptionModel)
	if cfg.Voice.TranscriptionModel == "" {
		cfg.Voice.TranscriptionModel = defaults.Voice.TranscriptionModel
	}
	cfg.Voice.OllamaBinary = strings.TrimSpace(cfg.Voice.OllamaBinary)
	if cfg.Voice.OllamaBinary == "" {
		cfg.Voice.OllamaBinary = defaults.Voice.OllamaBinary
	}
	cfg.Voice.FFmpegBinary = strings.TrimSpace(cfg.Voice.FFmpegBinary)
	if cfg.Voice.FFmpegBinary == "" {
		cfg.Voice.FFmpegBinary = defaults.Voice.FFmpegBinary
	}
	cfg.Voice.KokoroPython = strings.TrimSpace(cfg.Voice.KokoroPython)
	if cfg.Voice.KokoroPython == "" {
		cfg.Voice.KokoroPython = defaults.Voice.KokoroPython
	}
	cfg.Voice.KokoroVoice = strings.TrimSpace(cfg.Voice.KokoroVoice)
	if cfg.Voice.KokoroVoice == "" {
		cfg.Voice.KokoroVoice = defaults.Voice.KokoroVoice
	}
	cfg.Voice.KokoroLangCode = strings.TrimSpace(cfg.Voice.KokoroLangCode)
	if cfg.Voice.KokoroLangCode == "" {
		cfg.Voice.KokoroLangCode = defaults.Voice.KokoroLangCode
	}
	if cfg.Voice.KokoroSpeed <= 0 {
		cfg.Voice.KokoroSpeed = defaults.Voice.KokoroSpeed
	}
	if cfg.Voice.MaxSpeechChars <= 0 {
		cfg.Voice.MaxSpeechChars = defaults.Voice.MaxSpeechChars
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
	cfg.LocalControl.ListenAddr = strings.TrimSpace(cfg.LocalControl.ListenAddr)
	if cfg.LocalControl.ListenAddr == "" {
		cfg.LocalControl.ListenAddr = defaults.LocalControl.ListenAddr
	}
	cfg.LocalControl.TokenPath = strings.TrimSpace(cfg.LocalControl.TokenPath)
	if cfg.LocalControl.TokenPath == "" {
		cfg.LocalControl.TokenPath = defaults.LocalControl.TokenPath
	}
	cfg.Subagents.CodexBinary = strings.TrimSpace(cfg.Subagents.CodexBinary)
	if cfg.Subagents.CodexBinary == "" {
		cfg.Subagents.CodexBinary = defaults.Subagents.CodexBinary
	}
	cfg.Subagents.RootDir = strings.TrimSpace(cfg.Subagents.RootDir)
	if cfg.Subagents.RootDir == "" {
		cfg.Subagents.RootDir = defaults.Subagents.RootDir
	}
	if cfg.Subagents.MaxConcurrent <= 0 {
		cfg.Subagents.MaxConcurrent = defaults.Subagents.MaxConcurrent
	}
	if cfg.Subagents.MaxConcurrent > 12 {
		cfg.Subagents.MaxConcurrent = 12
	}
	if cfg.Subagents.DefaultTimeoutMinutes <= 0 {
		cfg.Subagents.DefaultTimeoutMinutes = defaults.Subagents.DefaultTimeoutMinutes
	}
	if cfg.Subagents.DefaultTimeoutMinutes > 24*60 {
		cfg.Subagents.DefaultTimeoutMinutes = 24 * 60
	}
	cfg.Subagents.DefaultModel = strings.TrimSpace(cfg.Subagents.DefaultModel)
	cfg.Subagents.DefaultProfile = strings.TrimSpace(cfg.Subagents.DefaultProfile)
	cfg.Subagents.DefaultReasoningEffort = normalizeReasoningEffort(cfg.Subagents.DefaultReasoningEffort)
	if cfg.Subagents.DefaultReasoningEffort == "" {
		cfg.Subagents.DefaultReasoningEffort = defaults.Subagents.DefaultReasoningEffort
	}
	cfg.Subagents.Sandbox = strings.TrimSpace(cfg.Subagents.Sandbox)
	if cfg.Subagents.Sandbox == "" {
		cfg.Subagents.Sandbox = defaults.Subagents.Sandbox
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
	cfg.Voice.KokoroPython, err = expandPath(cfg.Voice.KokoroPython)
	if err != nil {
		return Config{}, fmt.Errorf("expand kokoro python path: %w", err)
	}
	cfg.LocalControl.TokenPath, err = expandPath(cfg.LocalControl.TokenPath)
	if err != nil {
		return Config{}, fmt.Errorf("expand local control token path: %w", err)
	}
	cfg.Subagents.RootDir, err = expandPath(cfg.Subagents.RootDir)
	if err != nil {
		return Config{}, fmt.Errorf("expand subagents root dir: %w", err)
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
	cfg.Voice.KokoroPython, _ = expandPath(cfg.Voice.KokoroPython)
	cfg.LocalControl.TokenPath, _ = expandPath(cfg.LocalControl.TokenPath)
	cfg.Subagents.RootDir, _ = expandPath(cfg.Subagents.RootDir)
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

func normalizeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}
