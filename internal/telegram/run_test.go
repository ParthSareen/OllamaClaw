package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/agent"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
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
		{in: "/ show tools", want: "show"},
		{in: "/show tools", want: "show"},
		{in: "/ show thinking on", want: "show"},
		{in: "/show thinking off", want: "show"},
		{in: "/stop", want: "stop"},
		{in: "/restart", want: "restart"},
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

func TestParseThinkValue(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "on", want: "on", ok: true},
		{in: "off", want: "off", ok: true},
		{in: "low", want: "low", ok: true},
		{in: "medium", want: "medium", ok: true},
		{in: "high", want: "high", ok: true},
		{in: "default", want: "default", ok: true},
		{in: "auto", want: "default", ok: true},
		{in: "false", want: "off", ok: true},
		{in: "true", want: "on", ok: true},
		{in: "invalid", want: "", ok: false},
	}
	for _, tc := range tests {
		got, ok := parseThinkValue(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("parseThinkValue(%q) = (%q,%t), want (%q,%t)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestApprovalCallbackRoundTrip(t *testing.T) {
	data := formatApprovalCallback("allow", "abc123")
	action, id, ok := parseApprovalCallback(data)
	if !ok || action != "allow" || id != "abc123" {
		t.Fatalf("unexpected parse result: ok=%t action=%q id=%q", ok, action, id)
	}
	if _, _, ok := parseApprovalCallback("other:allow:abc123"); ok {
		t.Fatalf("expected invalid prefix to fail")
	}
	if _, _, ok := parseApprovalCallback("appr:approve:abc123"); ok {
		t.Fatalf("expected invalid action to fail")
	}
	data = formatApprovalCallback("always", "def456")
	action, id, ok = parseApprovalCallback(data)
	if !ok || action != "always" || id != "def456" {
		t.Fatalf("unexpected always parse result: ok=%t action=%q id=%q", ok, action, id)
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

func TestRunnerTurnLifecycle(t *testing.T) {
	r := &Runner{}
	canceled := false
	id, ok := r.beginTurn("8750063231", 8750063231, func() {
		canceled = true
	})
	if !ok || id == 0 {
		t.Fatalf("expected beginTurn to acquire turn")
	}
	if _, ok := r.beginTurn("8750063231", 8750063231, func() {}); ok {
		t.Fatalf("expected second beginTurn on same session to be rejected")
	}
	turn, ok := r.stopTurn("8750063231")
	if !ok {
		t.Fatalf("expected stopTurn to find in-flight turn")
	}
	if turn.id != id {
		t.Fatalf("stopTurn returned wrong turn id: got %d want %d", turn.id, id)
	}
	if !canceled {
		t.Fatalf("expected cancel func to be called")
	}
	r.endTurn("8750063231", id)
	if _, ok := r.stopTurn("8750063231"); ok {
		t.Fatalf("expected no in-flight turn after endTurn")
	}
}

func TestRunnerEndTurnRequiresMatchingID(t *testing.T) {
	r := &Runner{}
	id, ok := r.beginTurn("123", 123, func() {})
	if !ok {
		t.Fatalf("beginTurn failed")
	}
	r.endTurn("123", id+1)
	if _, ok := r.stopTurn("123"); !ok {
		t.Fatalf("turn should remain active when endTurn uses stale id")
	}
}

func TestRunnerRequestRestart(t *testing.T) {
	r := &Runner{}
	if r.requestRestart() {
		t.Fatalf("expected requestRestart without active run context to return false")
	}

	canceled := make(chan struct{}, 1)
	r.setRunCancel(func() {
		select {
		case canceled <- struct{}{}:
		default:
		}
	})

	if !r.requestRestart() {
		t.Fatalf("expected requestRestart to return true when run cancel is set")
	}
	if !r.restarting.Load() {
		t.Fatalf("expected restarting flag to be set")
	}
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected requestRestart to call run cancel")
	}
}

func TestResolvePendingApproval(t *testing.T) {
	r := &Runner{
		approvals: map[string]*pendingApproval{},
	}
	entry := &pendingApproval{
		ID:         "abc",
		ChatID:     100,
		UserID:     200,
		SessionKey: "100",
		Command:    "touch /tmp/x",
		Reason:     "outside allowlist",
		CreatedAt:  time.Now().UTC(),
		ExpiresAt:  time.Now().UTC().Add(time.Minute),
		DecisionCh: make(chan approvalDecision, 1),
	}
	r.approvals[entry.ID] = entry

	resolved, err := r.resolvePendingApproval(entry.ID, approvalDecisionAllow, 100, 200)
	if err != nil {
		t.Fatalf("resolvePendingApproval error: %v", err)
	}
	if resolved == nil || resolved.ID != entry.ID {
		t.Fatalf("unexpected resolved entry: %+v", resolved)
	}
	select {
	case decision := <-entry.DecisionCh:
		if decision != approvalDecisionAllow {
			t.Fatalf("expected allow decision, got %v", decision)
		}
	default:
		t.Fatalf("expected decision to be sent")
	}
	if _, exists := r.approvals[entry.ID]; exists {
		t.Fatalf("expected pending approval to be removed")
	}
}

func TestCallbackQueryChatInfo(t *testing.T) {
	cq := &models.CallbackQuery{
		Message: models.MaybeInaccessibleMessage{
			Type: models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{
				ID:   77,
				Chat: models.Chat{ID: 12345},
			},
		},
	}
	chatID, msgID, ok := callbackQueryChatInfo(cq)
	if !ok || chatID != 12345 || msgID != 77 {
		t.Fatalf("unexpected callbackQueryChatInfo result: ok=%t chat=%d msg=%d", ok, chatID, msgID)
	}
}

func TestUnauthorizedReplyCooldown(t *testing.T) {
	r := &Runner{}
	now := time.Unix(100, 0)

	if !r.shouldSendUnauthorizedReply(1, 2, now) {
		t.Fatalf("expected first unauthorized reply to be allowed")
	}
	if r.shouldSendUnauthorizedReply(1, 2, now.Add(unauthorizedReplyCooldown/2)) {
		t.Fatalf("expected repeated unauthorized reply to be throttled")
	}
	if !r.shouldSendUnauthorizedReply(1, 2, now.Add(unauthorizedReplyCooldown+time.Second)) {
		t.Fatalf("expected unauthorized reply after cooldown to be allowed again")
	}
	if !r.shouldSendUnauthorizedReply(1, 3, now.Add(unauthorizedReplyCooldown/2)) {
		t.Fatalf("expected different user to have independent cooldown")
	}
}

func TestFormatLiveToolEvent(t *testing.T) {
	start := formatLiveToolEvent(agent.ToolEvent{
		Phase:    agent.ToolEventStart,
		Index:    1,
		Name:     "read_file",
		ArgsJSON: `{"path":"a.txt"}`,
	})
	if !strings.Contains(start, "tool start 1") || !strings.Contains(start, "read_file") {
		t.Fatalf("unexpected start event format: %q", start)
	}
	done := formatLiveToolEvent(agent.ToolEvent{
		Phase:      agent.ToolEventFinish,
		Index:      2,
		Name:       "bash",
		ArgsJSON:   `{"command":"git remote show origin"}`,
		DurationMs: 12,
		ResultJSON: `{"exit_code":0}`,
	})
	if !strings.Contains(done, "tool done 2") || !strings.Contains(done, "result=") {
		t.Fatalf("unexpected finish event format: %q", done)
	}
	if !strings.Contains(done, "git remote show origin") {
		t.Fatalf("expected bash command preview in finish event, got %q", done)
	}
	errLine := formatLiveToolEvent(agent.ToolEvent{
		Phase:      agent.ToolEventFinish,
		Index:      3,
		Name:       "bash",
		ArgsJSON:   `{"command":"sleep 10"}`,
		DurationMs: 2,
		Error:      "context canceled",
	})
	if !strings.Contains(errLine, "error=context canceled") {
		t.Fatalf("unexpected error event format: %q", errLine)
	}
	if !strings.Contains(errLine, "sleep 10") {
		t.Fatalf("expected bash command preview in error event, got %q", errLine)
	}
}

func TestFormatThinkingTrace(t *testing.T) {
	trace := []agent.ThinkingTraceEntry{
		{Step: 1, Thinking: " plan first, then run tools ", ToolCallCount: 2},
		{Step: 2, Thinking: "finalize answer", ToolCallCount: 0},
	}
	out := formatThinkingTrace(trace)
	if !strings.Contains(out, "thinking trace:") {
		t.Fatalf("missing thinking trace header: %q", out)
	}
	if !strings.Contains(out, "step=1") || !strings.Contains(out, "tool-step (2 tool calls)") {
		t.Fatalf("missing step/tool-step details: %q", out)
	}
	if !strings.Contains(out, "step=2") || !strings.Contains(out, "final") {
		t.Fatalf("missing final-step details: %q", out)
	}
}

func TestIsContextCanceledErr(t *testing.T) {
	if !isContextCanceledErr(context.Canceled) {
		t.Fatalf("expected context.Canceled to be treated as canceled")
	}
	if !isContextCanceledErr(errors.New("request failed: context canceled while reading body")) {
		t.Fatalf("expected textual context canceled error to match")
	}
	if isContextCanceledErr(errors.New("network timeout")) {
		t.Fatalf("unexpected match for non-cancel error")
	}
}
