package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parth/ollamaclaw/internal/ollama"
)

func TestTruncate(t *testing.T) {
	in := "abcdefghijklmnopqrstuvwxyz"
	out := truncate(in, 10)
	if len(out) > 10 {
		t.Fatalf("expected truncated output <= 10, got %d", len(out))
	}
}

func TestAsInt(t *testing.T) {
	if v, ok := asInt(float64(3)); !ok || v != 3 {
		t.Fatalf("asInt(float64) failed")
	}
	if _, ok := asInt("3"); ok {
		t.Fatalf("asInt should fail for string")
	}
}

func TestReadLogsTool(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ollamaclaw.log")
	content := strings.Join([]string{
		"[a] one",
		"[b] two",
		"[a] three",
		"[c] four",
	}, "\n")
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write log file: %v", err)
	}

	toolMap := ToolMap(BuiltinTools(BuiltinsConfig{
		ToolOutputMaxBytes: 4096,
		BashTimeoutSec:     10,
		LogPath:            logPath,
	}, ollama.NewClient("http://localhost:11434")))
	readLogs, ok := toolMap["read_logs"]
	if !ok {
		t.Fatalf("read_logs tool not found")
	}

	res, err := readLogs.Execute(context.Background(), map[string]interface{}{
		"lines":    2,
		"contains": "[a]",
	})
	if err != nil {
		t.Fatalf("read_logs execute error: %v", err)
	}
	if got, _ := res["selected_lines"].(int); got != 2 {
		t.Fatalf("expected selected_lines=2, got %#v", res["selected_lines"])
	}
	body, _ := res["content"].(string)
	if !strings.Contains(body, "[a] one") || !strings.Contains(body, "[a] three") {
		t.Fatalf("unexpected content %q", body)
	}
	if strings.Contains(body, "[b] two") {
		t.Fatalf("expected filtered output, got %q", body)
	}
}

func TestGuardTelegramBashCommand(t *testing.T) {
	telegramCtx := WithSessionInfo(context.Background(), "telegram", "8750063231")
	if err := guardTelegramBashCommand(telegramCtx, "ps aux | grep ollamaclaw"); err != nil {
		t.Fatalf("expected read-only command to be allowed, got %v", err)
	}

	blocked := []string{
		"./ollamaclaw telegram run > /tmp/ollamaclaw.log 2>&1 &",
		"pkill -f 'ollamaclaw telegram run' || true",
		"killall ollamaclaw",
		"rm -f ~/.ollamaclaw/launch.lock",
	}
	for _, cmd := range blocked {
		if err := guardTelegramBashCommand(telegramCtx, cmd); err == nil {
			t.Fatalf("expected command to be blocked: %q", cmd)
		}
	}

	replCtx := WithSessionInfo(context.Background(), "repl", "default")
	if err := guardTelegramBashCommand(replCtx, "./ollamaclaw launch"); err != nil {
		t.Fatalf("expected repl context to allow command, got %v", err)
	}
}
