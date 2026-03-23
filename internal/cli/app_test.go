package cli

import (
	"strings"
	"testing"

	"github.com/parth/ollamaclaw/internal/config"
)

func TestLaunchRejectsArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLLAMACLAW_FORCE_NONINTERACTIVE", "1")
	app := New()
	err := app.Run([]string{"launch", "extra"})
	if err == nil || !strings.Contains(err.Error(), "launch takes no arguments") {
		t.Fatalf("expected launch arg validation error, got %v", err)
	}
}

func TestLaunchRequiresToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLLAMACLAW_FORCE_NONINTERACTIVE", "1")
	app := New()
	err := app.Run([]string{"launch"})
	if err == nil || !strings.Contains(err.Error(), "telegram bot token is missing") {
		t.Fatalf("expected missing token error, got %v", err)
	}
}

func TestLaunchRequiresAllowlist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLLAMACLAW_FORCE_NONINTERACTIVE", "1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Telegram.BotToken = "test-token"
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	app := New()
	err = app.Run([]string{"launch"})
	if err == nil || !strings.Contains(err.Error(), "telegram owner allowlist is missing") {
		t.Fatalf("expected missing allowlist error, got %v", err)
	}
}

func TestTelegramRunAliasToLaunch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLLAMACLAW_FORCE_NONINTERACTIVE", "1")
	app := New()
	err := app.Run([]string{"telegram", "run"})
	if err == nil || !strings.Contains(err.Error(), "telegram bot token is missing") {
		t.Fatalf("expected launch-path missing token error, got %v", err)
	}
}

func TestTelegramRunAliasRejectsArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLLAMACLAW_FORCE_NONINTERACTIVE", "1")
	app := New()
	err := app.Run([]string{"telegram", "run", "extra"})
	if err == nil || !strings.Contains(err.Error(), "launch takes no arguments") {
		t.Fatalf("expected launch arg validation error via alias, got %v", err)
	}
}

func TestNormalizeOwnerIDs(t *testing.T) {
	tests := []struct {
		name        string
		ownerID     int64
		ownerChatID int64
		ownerUserID int64
		wantChatID  int64
		wantUserID  int64
	}{
		{
			name:        "owner-id only sets both",
			ownerID:     123,
			ownerChatID: 0,
			ownerUserID: 0,
			wantChatID:  123,
			wantUserID:  123,
		},
		{
			name:        "chat only mirrors to user",
			ownerID:     0,
			ownerChatID: 222,
			ownerUserID: 0,
			wantChatID:  222,
			wantUserID:  222,
		},
		{
			name:        "user only mirrors to chat",
			ownerID:     0,
			ownerChatID: 0,
			ownerUserID: 333,
			wantChatID:  333,
			wantUserID:  333,
		},
		{
			name:        "explicit chat and user preserved",
			ownerID:     0,
			ownerChatID: 444,
			ownerUserID: 555,
			wantChatID:  444,
			wantUserID:  555,
		},
		{
			name:        "owner-id fills only missing side",
			ownerID:     777,
			ownerChatID: 888,
			ownerUserID: 0,
			wantChatID:  888,
			wantUserID:  777,
		},
		{
			name:        "all zero stays zero",
			ownerID:     0,
			ownerChatID: 0,
			ownerUserID: 0,
			wantChatID:  0,
			wantUserID:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotChatID, gotUserID := normalizeOwnerIDs(tc.ownerID, tc.ownerChatID, tc.ownerUserID)
			if gotChatID != tc.wantChatID || gotUserID != tc.wantUserID {
				t.Fatalf("normalizeOwnerIDs(%d,%d,%d) = (%d,%d), want (%d,%d)", tc.ownerID, tc.ownerChatID, tc.ownerUserID, gotChatID, gotUserID, tc.wantChatID, tc.wantUserID)
			}
		})
	}
}

func TestRunNoArgsDefaultsToLaunch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLLAMACLAW_FORCE_NONINTERACTIVE", "1")
	app := New()
	err := app.Run(nil)
	if err == nil || !strings.Contains(err.Error(), "telegram bot token is missing") {
		t.Fatalf("expected default launch missing-token error, got %v", err)
	}
}

func TestConfigureRequiresInteractiveTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OLLAMACLAW_FORCE_NONINTERACTIVE", "1")
	app := New()
	err := app.Run([]string{"configure"})
	if err == nil || !strings.Contains(err.Error(), "configure requires an interactive terminal") {
		t.Fatalf("expected non-interactive configure error, got %v", err)
	}
}

func TestAcquireLaunchLockPreventsDuplicate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	lockPath, release, err := acquireLaunchLock()
	if err != nil {
		t.Fatalf("first lock acquisition failed: %v", err)
	}
	if strings.TrimSpace(lockPath) == "" {
		t.Fatalf("expected non-empty lock path")
	}

	_, release2, err := acquireLaunchLock()
	if err == nil {
		if release2 != nil {
			release2()
		}
		t.Fatalf("expected duplicate lock acquisition to fail")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected friendly duplicate lock error, got %v", err)
	}

	release()
	_, release3, err := acquireLaunchLock()
	if err != nil {
		t.Fatalf("expected lock to be reacquirable after release: %v", err)
	}
	release3()
}

func TestParseLaunchProcessConflicts(t *testing.T) {
	ps := strings.Join([]string{
		"100 99 /bin/bash -lc rm -f ~/.ollamaclaw/launch.lock; ./ollamaclaw telegram run > /tmp/ollamaclaw.log 2>&1 &",
		"101 1 ./ollamaclaw telegram run",
		"102 201 ./ollamaclaw",
		"201 301 timeout 8 ./ollamaclaw",
		"301 1 /bin/zsh -lc ./ollamaclaw",
		"103 1 pgrep -af ollamaclaw",
		"104 1 grep ollamaclaw /tmp/file",
		"105 1 /usr/bin/python worker.py",
		"",
	}, "\n")
	got := parseLaunchProcessConflicts(ps, 102)
	if len(got) != 2 {
		t.Fatalf("expected 2 conflicts after filtering self and grep-like commands, got %d (%v)", len(got), got)
	}
	if !strings.Contains(got[0], "pid=100") {
		t.Fatalf("unexpected first conflict: %s", got[0])
	}
	if !strings.Contains(got[1], "pid=101") {
		t.Fatalf("unexpected second conflict: %s", got[1])
	}
}

func TestPreviewCommandForError(t *testing.T) {
	if got := previewCommandForError("  ./ollamaclaw   telegram   run "); got != "./ollamaclaw telegram run" {
		t.Fatalf("unexpected compacted command: %q", got)
	}
	long := strings.Repeat("x", 250)
	got := previewCommandForError(long)
	if len(got) != 180 || !strings.HasSuffix(got, "...") {
		t.Fatalf("expected truncated preview of length 180 with ellipsis, got len=%d value=%q", len(got), got)
	}
}
