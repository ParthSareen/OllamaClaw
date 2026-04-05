package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ParthSareen/OllamaClaw/internal/ollama"
)

type stubBashApprover struct {
	lastReq BashApprovalRequest
	err     error
	called  bool
}

func (s *stubBashApprover) ApproveBashCommand(ctx context.Context, req BashApprovalRequest) error {
	_ = ctx
	s.called = true
	s.lastReq = req
	return s.err
}

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
	if err := guardTelegramBashCommand(telegramCtx, "ps aux"); err != nil {
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

func TestGuardTelegramBashCommandRequiresApproval(t *testing.T) {
	ctx := WithSessionInfo(context.Background(), "telegram", "8750063231")
	err := guardTelegramBashCommand(ctx, "touch /tmp/test-file")
	if err == nil || !strings.Contains(err.Error(), "requires approval") {
		t.Fatalf("expected approval-required error, got %v", err)
	}
}

func TestGuardTelegramBashCommandApproverAllows(t *testing.T) {
	approver := &stubBashApprover{}
	ctx := WithSessionInfo(context.Background(), "telegram", "8750063231")
	ctx = WithBashApprover(ctx, approver)
	if err := guardTelegramBashCommand(ctx, "touch /tmp/test-file"); err != nil {
		t.Fatalf("expected approver to allow command, got %v", err)
	}
	if !approver.called {
		t.Fatalf("expected approver to be called")
	}
	if strings.TrimSpace(approver.lastReq.Command) != "touch /tmp/test-file" {
		t.Fatalf("unexpected approval command: %q", approver.lastReq.Command)
	}
	if approver.lastReq.Reason == "" {
		t.Fatalf("expected approval reason")
	}
	if !approver.lastReq.AllowAlways {
		t.Fatalf("expected non-network command to support always-allow")
	}
}

func TestGuardTelegramBashCommandApproverDenies(t *testing.T) {
	approver := &stubBashApprover{err: errors.New("denied by user")}
	ctx := WithSessionInfo(context.Background(), "telegram", "8750063231")
	ctx = WithBashApprover(ctx, approver)
	err := guardTelegramBashCommand(ctx, "touch /tmp/test-file")
	if err == nil || !strings.Contains(err.Error(), "denied by user") {
		t.Fatalf("expected deny error from approver, got %v", err)
	}
}

func TestClassifyTelegramBashCommand(t *testing.T) {
	cases := []struct {
		cmd      string
		want     telegramBashPolicy
		reasonIn string
	}{
		{cmd: "ls -la", want: telegramBashPolicyAllow},
		{cmd: "git status", want: telegramBashPolicyAllow},
		{cmd: "ollama list", want: telegramBashPolicyAllow},
		{cmd: "curl https://example.com", want: telegramBashPolicyRequireApproval, reasonIn: "network/data"},
		{cmd: "touch /tmp/x", want: telegramBashPolicyRequireApproval, reasonIn: "outside"},
		{cmd: "sudo ls", want: telegramBashPolicyDeny, reasonIn: "denied"},
		{cmd: "rm -f ~/.ollamaclaw/launch.lock", want: telegramBashPolicyDeny, reasonIn: "lock files"},
	}
	for _, tc := range cases {
		got, reason := classifyTelegramBashCommand(normalizeTelegramBashCommand(tc.cmd))
		if got != tc.want {
			t.Fatalf("classify(%q)=%v want=%v (reason=%q)", tc.cmd, got, tc.want, reason)
		}
		if tc.reasonIn != "" && !strings.Contains(strings.ToLower(reason), strings.ToLower(tc.reasonIn)) {
			t.Fatalf("classify(%q) reason=%q expected to contain %q", tc.cmd, reason, tc.reasonIn)
		}
	}
}

func TestGuardTelegramBashCommandCurlAlwaysNeedsApproval(t *testing.T) {
	ctx := WithSessionInfo(context.Background(), "telegram", "8750063231")
	err := guardTelegramBashCommand(ctx, "curl https://example.com")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "requires approval") {
		t.Fatalf("expected curl command to require approval, got %v", err)
	}
}

func TestGuardTelegramBashCommandCurlEnablesAlwaysAllow(t *testing.T) {
	approver := &stubBashApprover{}
	ctx := WithSessionInfo(context.Background(), "telegram", "8750063231")
	ctx = WithBashApprover(ctx, approver)
	if err := guardTelegramBashCommand(ctx, "curl https://example.com"); err != nil {
		t.Fatalf("expected approver path for curl, got %v", err)
	}
	if !approver.called {
		t.Fatalf("expected approver to be called")
	}
	if !approver.lastReq.AllowAlways {
		t.Fatalf("expected curl approvals to allow always-allow")
	}
}

func TestGuardTelegramBashCommandControlOperatorsEnableAlwaysAllow(t *testing.T) {
	approver := &stubBashApprover{}
	ctx := WithSessionInfo(context.Background(), "telegram", "8750063231")
	ctx = WithBashApprover(ctx, approver)
	cmd := "tail -100 ~/.codex/history.jsonl 2>/dev/null | head -30"
	if err := guardTelegramBashCommand(ctx, cmd); err != nil {
		t.Fatalf("expected approver path for control-operator command, got %v", err)
	}
	if !approver.called {
		t.Fatalf("expected approver to be called")
	}
	if !approver.lastReq.AllowAlways {
		t.Fatalf("expected control-operator approvals to allow always-allow")
	}
	if !strings.Contains(strings.ToLower(approver.lastReq.Reason), "shell control operators") {
		t.Fatalf("expected control-operator reason, got %q", approver.lastReq.Reason)
	}
}

func TestEffectiveBashTimeoutSec(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		args       map[string]interface{}
		want       int
	}{
		{name: "default when configured zero", configured: 0, args: map[string]interface{}{}, want: 120},
		{name: "clamp configured high", configured: 900, args: map[string]interface{}{}, want: 120},
		{name: "use arg timeout", configured: 60, args: map[string]interface{}{"timeout_seconds": float64(30)}, want: 30},
		{name: "clamp arg timeout high", configured: 60, args: map[string]interface{}{"timeout_seconds": float64(999)}, want: 120},
		{name: "arg timeout ignored when non-positive", configured: 45, args: map[string]interface{}{"timeout_seconds": float64(0)}, want: 45},
	}
	for _, tc := range tests {
		if got := effectiveBashTimeoutSec(tc.configured, tc.args); got != tc.want {
			t.Fatalf("%s: effectiveBashTimeoutSec(%d, %+v)=%d want=%d", tc.name, tc.configured, tc.args, got, tc.want)
		}
	}
}

func TestBashToolTimeoutMessageIncludesDuration(t *testing.T) {
	toolMap := ToolMap(BuiltinTools(BuiltinsConfig{
		ToolOutputMaxBytes: 4096,
		BashTimeoutSec:     10,
	}, ollama.NewClient("http://localhost:11434")))
	bashTool, ok := toolMap["bash"]
	if !ok {
		t.Fatalf("bash tool not found")
	}

	res, err := bashTool.Execute(context.Background(), map[string]interface{}{
		"command":         "sleep 2",
		"timeout_seconds": float64(1),
	})
	if err != nil {
		t.Fatalf("bash execute error: %v", err)
	}
	if got, ok := res["exit_code"].(int); !ok || got != -1 {
		t.Fatalf("expected exit_code -1 on timeout, got %#v", res["exit_code"])
	}
	stderr, _ := res["stderr"].(string)
	if !strings.Contains(stderr, "command timed out after 1s") {
		t.Fatalf("expected timeout duration in stderr, got %q", stderr)
	}
}
