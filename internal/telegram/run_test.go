package telegram

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
)

func TestSplitText(t *testing.T) {
	text := "line1\nline2\nline3\nline4"
	chunks := splitText(text, 8)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks")
	}
	for _, c := range chunks {
		if len(c) > 8 {
			t.Fatalf("chunk too long: %d", len(c))
		}
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "/help", want: "help"},
		{in: "/help@my_bot", want: "help"},
		{in: " /model kimi-k2.5:cloud ", want: "model"},
		{in: "plain text", want: ""},
		{in: "", want: ""},
	}
	for _, tc := range tests {
		if got := parseCommand(tc.in); got != tc.want {
			t.Fatalf("parseCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseOnOff(t *testing.T) {
	onInputs := []string{"on", "1", "true", "yes", " ON "}
	for _, in := range onInputs {
		got, ok := parseOnOff(in)
		if !ok || !got {
			t.Fatalf("parseOnOff(%q) = (%t,%t), want (true,true)", in, got, ok)
		}
	}
	offInputs := []string{"off", "0", "false", "no", " off "}
	for _, in := range offInputs {
		got, ok := parseOnOff(in)
		if !ok || got {
			t.Fatalf("parseOnOff(%q) = (%t,%t), want (false,true)", in, got, ok)
		}
	}
	if _, ok := parseOnOff("maybe"); ok {
		t.Fatalf("parseOnOff should reject unknown input")
	}
}

func TestPreviewForLog(t *testing.T) {
	if got := previewForLog(" \n\t "); got != "" {
		t.Fatalf("expected empty preview for whitespace, got %q", got)
	}
	got := previewForLog("a   b\nc")
	if got != "a b c" {
		t.Fatalf("expected compacted whitespace, got %q", got)
	}
	long := strings.Repeat("x", maxLogPreview+50)
	out := previewForLog(long)
	if len(out) != maxLogPreview {
		t.Fatalf("expected preview length %d, got %d", maxLogPreview, len(out))
	}
	if !strings.HasSuffix(out, "...") {
		t.Fatalf("expected ellipsis suffix, got %q", out)
	}
}

func TestIsPollingConflictErr(t *testing.T) {
	conflictWrapped := fmt.Errorf("polling failed: %w", bot.ErrorConflict)
	if !isPollingConflictErr(conflictWrapped) {
		t.Fatalf("expected wrapped bot.ErrorConflict to be detected")
	}

	conflictString := errors.New("telegram error: Conflict: terminated by other getUpdates request; make sure that only one bot instance is running")
	if !isPollingConflictErr(conflictString) {
		t.Fatalf("expected getUpdates conflict text to be detected")
	}

	nonConflict := errors.New("telegram error: bad request: chat not found")
	if isPollingConflictErr(nonConflict) {
		t.Fatalf("did not expect non-conflict error to be detected")
	}
}

func TestParsePollerCandidates(t *testing.T) {
	ps := strings.Join([]string{
		"123 /usr/bin/ssh-agent -l",
		"222 ./ollamaclaw telegram run",
		"333 ./ollamaclaw",
		"444 /Users/parth/bin/ollamaclaw launch",
		"445 bun run --cwd /Users/parth/.claude/plugins/cache/claude-plugins-official/telegram/0.0.1 --shell=bun --silent start",
		"555 pgrep -af ollamaclaw",
		"",
	}, "\n")
	got := parsePollerCandidates(ps, 333)
	if len(got) != 3 {
		t.Fatalf("expected 3 candidates after filtering self pid, got %d (%+v)", len(got), got)
	}
	if got[0].pid != 222 || !strings.Contains(got[0].cmd, "telegram run") {
		t.Fatalf("unexpected first candidate: %+v", got[0])
	}
	if got[1].pid != 444 || !strings.Contains(got[1].cmd, "launch") {
		t.Fatalf("unexpected second candidate: %+v", got[1])
	}
	if got[2].pid != 445 || !strings.Contains(got[2].cmd, "plugins-official/telegram") {
		t.Fatalf("unexpected third candidate: %+v", got[2])
	}
}
