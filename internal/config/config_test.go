package config

import (
	"os"
	"path/filepath"
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
	path, _ := ConfigPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected config file to exist: %v", err)
	}
}

func TestRedactedMasksToken(t *testing.T) {
	cfg := Default()
	cfg.Telegram.BotToken = "secret"
	red := Redacted(cfg)
	if red.Telegram.BotToken != "***" {
		t.Fatalf("expected redacted token, got %q", red.Telegram.BotToken)
	}
}

func TestSaveExpandsDBPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Default()
	cfg.DBPath = "~/custom/state.db"
	cfg.LogPath = "~/custom/ollamaclaw.log"
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
}
