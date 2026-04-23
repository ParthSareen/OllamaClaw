package telegram

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/agent"
	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type fakeTelegramFileClient struct {
	files map[string]*models.File
	base  string
	err   error
}

func (f fakeTelegramFileClient) GetFile(ctx context.Context, params *bot.GetFileParams) (*models.File, error) {
	_ = ctx
	if f.err != nil {
		return nil, f.err
	}
	if file, ok := f.files[params.FileID]; ok {
		cp := *file
		return &cp, nil
	}
	return nil, fmt.Errorf("unknown file id %s", params.FileID)
}

func (f fakeTelegramFileClient) FileDownloadLink(file *models.File) string {
	return strings.TrimRight(f.base, "/") + "/" + strings.TrimLeft(file.FilePath, "/")
}

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
		{in: "/reminder list", want: "reminder"},
		{in: "/dream", want: "dream"},
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

func TestExtractMessageInputPrefersTextThenCaptionAndImageFallback(t *testing.T) {
	msg := &models.Message{Text: "hello"}
	text, images := extractMessageInput(msg)
	if text != "hello" || len(images) != 0 {
		t.Fatalf("expected plain text input, got text=%q images=%v", text, images)
	}

	msg = &models.Message{Caption: "check this", Photo: []models.PhotoSize{{FileID: "small", FileSize: 10, Width: 10, Height: 10}, {FileID: "large", FileSize: 99, Width: 200, Height: 200}}}
	text, images = extractMessageInput(msg)
	if text != "check this" {
		t.Fatalf("expected caption text, got %q", text)
	}
	if len(images) != 1 || images[0] != "large" {
		t.Fatalf("expected largest photo file id, got %v", images)
	}

	msg = &models.Message{Photo: []models.PhotoSize{{FileID: "only", FileSize: 5, Width: 20, Height: 20}}}
	text, images = extractMessageInput(msg)
	if text != "Please analyze this image." {
		t.Fatalf("expected default text for image-only message, got %q", text)
	}
	if len(images) != 1 || images[0] != "only" {
		t.Fatalf("expected image-only file id, got %v", images)
	}
}

func TestExtractMessageInputIncludesImageDocument(t *testing.T) {
	msg := &models.Message{
		Caption: "doc image",
		Document: &models.Document{
			FileID:   "doc-image",
			MimeType: "image/png",
		},
	}
	text, images := extractMessageInput(msg)
	if text != "doc image" {
		t.Fatalf("expected caption text, got %q", text)
	}
	if len(images) != 1 || images[0] != "doc-image" {
		t.Fatalf("expected image document file id, got %v", images)
	}

	msg.Document.MimeType = "application/pdf"
	_, images = extractMessageInput(msg)
	if len(images) != 0 {
		t.Fatalf("expected non-image document to be ignored, got %v", images)
	}
}

func TestFetchTelegramImagesSuccess(t *testing.T) {
	body := []byte("fake-image-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file.jpg" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	api := fakeTelegramFileClient{
		base: srv.URL,
		files: map[string]*models.File{
			"file-1": {FileID: "file-1", FilePath: "file.jpg"},
		},
	}
	images, err := fetchTelegramImages(context.Background(), api, []string{"file-1"})
	if err != nil {
		t.Fatalf("fetchTelegramImages() error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected one image payload, got %d", len(images))
	}
	decoded, err := base64.StdEncoding.DecodeString(images[0])
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(decoded) != string(body) {
		t.Fatalf("decoded payload mismatch: got %q want %q", string(decoded), string(body))
	}
}

func TestFetchTelegramImagesTooLarge(t *testing.T) {
	large := strings.Repeat("a", telegramImageMaxBytes+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(large))
	}))
	defer srv.Close()

	api := fakeTelegramFileClient{
		base: srv.URL,
		files: map[string]*models.File{
			"big": {FileID: "big", FilePath: "big.jpg"},
		},
	}
	_, err := fetchTelegramImages(context.Background(), api, []string{"big"})
	if err == nil {
		t.Fatalf("expected too-large image error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "too large") {
		t.Fatalf("expected too-large error, got %v", err)
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

func TestFlushQueuedUpdatesAdvancesOffsetAndDropsQueuedMessages(t *testing.T) {
	oldCall := telegramAPICall
	t.Cleanup(func() { telegramAPICall = oldCall })

	fullBatch := make([]map[string]int64, 0, startupFlushBatchLimit)
	for i := 0; i < startupFlushBatchLimit; i++ {
		fullBatch = append(fullBatch, map[string]int64{"update_id": int64(101 + i)})
	}
	fullBatchJSON, err := json.Marshal(fullBatch)
	if err != nil {
		t.Fatalf("marshal full batch: %v", err)
	}

	type step struct {
		wantOffset int64
		result     string
	}
	steps := []step{
		{wantOffset: 51, result: string(fullBatchJSON)},
		{wantOffset: 201, result: `[{"update_id":201}]`},
	}
	callIdx := 0
	telegramAPICall = func(ctx context.Context, token, method string, payload interface{}) (json.RawMessage, error) {
		_ = ctx
		if token != "token" {
			t.Fatalf("unexpected token: %q", token)
		}
		if method != "getUpdates" {
			t.Fatalf("unexpected method: %q", method)
		}
		if callIdx >= len(steps) {
			t.Fatalf("unexpected extra getUpdates call #%d", callIdx+1)
		}
		args, ok := payload.(map[string]interface{})
		if !ok {
			t.Fatalf("unexpected payload type: %T", payload)
		}
		gotOffset, ok := args["offset"].(int64)
		if !ok {
			t.Fatalf("offset not int64: %#v", args["offset"])
		}
		if gotOffset != steps[callIdx].wantOffset {
			t.Fatalf("call %d offset=%d want=%d", callIdx+1, gotOffset, steps[callIdx].wantOffset)
		}
		callIdx++
		return json.RawMessage(steps[callIdx-1].result), nil
	}

	r := &Runner{Cfg: configForTestToken("token")}
	next, dropped, err := r.flushQueuedUpdates(context.Background(), 50)
	if err != nil {
		t.Fatalf("flushQueuedUpdates() error: %v", err)
	}
	if next != 201 {
		t.Fatalf("expected next offset 201, got %d", next)
	}
	if dropped != 101 {
		t.Fatalf("expected dropped updates count 101, got %d", dropped)
	}
	if callIdx != len(steps) {
		t.Fatalf("expected %d calls, got %d", len(steps), callIdx)
	}
}

func TestFlushQueuedUpdatesEmptyBacklog(t *testing.T) {
	oldCall := telegramAPICall
	t.Cleanup(func() { telegramAPICall = oldCall })

	telegramAPICall = func(ctx context.Context, token, method string, payload interface{}) (json.RawMessage, error) {
		_ = ctx
		_ = token
		_ = method
		_ = payload
		return json.RawMessage(`[]`), nil
	}
	r := &Runner{Cfg: configForTestToken("token")}
	next, dropped, err := r.flushQueuedUpdates(context.Background(), 777)
	if err != nil {
		t.Fatalf("flushQueuedUpdates() error: %v", err)
	}
	if next != 777 {
		t.Fatalf("expected unchanged offset 777, got %d", next)
	}
	if dropped != 0 {
		t.Fatalf("expected dropped=0, got %d", dropped)
	}
}

func TestFlushQueuedUpdatesDecodeError(t *testing.T) {
	oldCall := telegramAPICall
	t.Cleanup(func() { telegramAPICall = oldCall })

	telegramAPICall = func(ctx context.Context, token, method string, payload interface{}) (json.RawMessage, error) {
		_ = ctx
		_ = token
		_ = method
		_ = payload
		return json.RawMessage(`{not-json`), nil
	}
	r := &Runner{Cfg: configForTestToken("token")}
	_, _, err := r.flushQueuedUpdates(context.Background(), 1)
	if err == nil {
		t.Fatalf("expected decode error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "decode") {
		t.Fatalf("expected decode error context, got %v", err)
	}
}

func configForTestToken(token string) config.Config {
	cfg := config.Config{}
	cfg.Telegram.BotToken = token
	return cfg
}

func TestParsePollerCandidates(t *testing.T) {
	ps := strings.Join([]string{
		"123 /usr/bin/ssh-agent -l",
		"222 ./ollamaclaw telegram run",
		"333 ./ollamaclaw",
		"444 /Users/parth/bin/ollamaclaw launch",
		"445 bun run --cwd /Users/parth/.claude/agents/telegram/0.0.1 --shell=bun --silent start telegram bot",
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
	if got[2].pid != 445 || !strings.Contains(got[2].cmd, "agents/telegram") {
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

func TestRunnerQueuedTurnDebounceCoalescesMessages(t *testing.T) {
	r := &Runner{}
	executed := make(chan string, 4)
	r.turnExecutor = func(ctx context.Context, turnCtx context.Context, b *bot.Bot, chatID, userID int64, sessionKey, text string, imageFileIDs []string) {
		executed <- text
	}

	turnID, ok := r.beginTurn("8750063231", 8750063231, func() {})
	if !ok {
		t.Fatalf("expected initial in-flight turn")
	}

	first := r.enqueuePendingTurn("8750063231", pendingTurn{
		chatID:     8750063231,
		userID:     8750063231,
		sessionKey: "8750063231",
		text:       "first",
	})
	r.schedulePendingTurnDrain("8750063231", first.generation)

	time.Sleep(200 * time.Millisecond)

	second := r.enqueuePendingTurn("8750063231", pendingTurn{
		chatID:     8750063231,
		userID:     8750063231,
		sessionKey: "8750063231",
		text:       "second",
	})
	r.schedulePendingTurnDrain("8750063231", second.generation)
	r.endTurn("8750063231", turnID)

	select {
	case got := <-executed:
		t.Fatalf("queued turn ran too early before debounce elapsed: %q", got)
	case <-time.After(pendingTurnDebounce - 250*time.Millisecond):
	}

	select {
	case got := <-executed:
		if got != "first\nsecond" {
			t.Fatalf("expected coalesced queued message to run, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for queued turn to run")
	}

	select {
	case got := <-executed:
		t.Fatalf("expected only one queued execution, got extra %q", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRunnerQueuedTurnDebounceFirstMessageWaits(t *testing.T) {
	r := &Runner{}
	executed := make(chan string, 2)
	r.turnExecutor = func(ctx context.Context, turnCtx context.Context, b *bot.Bot, chatID, userID int64, sessionKey, text string, imageFileIDs []string) {
		executed <- text
	}

	queued := r.enqueuePendingTurn("8750063231", pendingTurn{
		chatID:     8750063231,
		userID:     8750063231,
		sessionKey: "8750063231",
		text:       "hello",
	})
	r.schedulePendingTurnDrain("8750063231", queued.generation)

	select {
	case got := <-executed:
		t.Fatalf("expected first message to wait for debounce, got immediate execution: %q", got)
	case <-time.After(pendingTurnDebounce - 250*time.Millisecond):
	}

	select {
	case got := <-executed:
		if got != "hello" {
			t.Fatalf("expected queued first message to run after debounce, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for first queued turn to run")
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

func TestFormatCompactionNotice(t *testing.T) {
	notice := formatCompactionNotice(205120, 201600, 8, compactionSnapshot{
		TotalCount: 7,
		LastAt:     "2026-04-15T10:30:00-07:00",
	})
	if !strings.Contains(notice, "context compacted:") {
		t.Fatalf("missing compaction header: %q", notice)
	}
	if !strings.Contains(notice, "prompt_tokens: 205120") {
		t.Fatalf("missing prompt_tokens field: %q", notice)
	}
	if !strings.Contains(notice, "threshold_tokens: 201600") {
		t.Fatalf("missing threshold_tokens field: %q", notice)
	}
	if !strings.Contains(notice, "keep_recent_turns: 8") {
		t.Fatalf("missing keep_recent_turns field: %q", notice)
	}
	if !strings.Contains(notice, "compactions_total: 7") {
		t.Fatalf("missing compactions_total field: %q", notice)
	}
	if !strings.Contains(notice, "last_compaction_at: 2026-04-15T10:30:00-07:00") {
		t.Fatalf("missing last_compaction_at field: %q", notice)
	}
}

func TestFormatCoreMemoryEvent(t *testing.T) {
	start := formatCoreMemoryEvent(agent.CoreMemoryEvent{
		Phase:         agent.CoreMemoryEventStart,
		UserTurnCount: 10,
		Model:         "kimi-k2.5:cloud",
	})
	if !strings.Contains(start, "dreaming started:") {
		t.Fatalf("missing start header: %q", start)
	}
	if !strings.Contains(start, "user_turns: 10") {
		t.Fatalf("missing user turn count: %q", start)
	}

	done := formatCoreMemoryEvent(agent.CoreMemoryEvent{
		Phase:         agent.CoreMemoryEventDone,
		UserTurnCount: 20,
		Model:         "kimi-k2.5:cloud",
		DurationMs:    1234,
		Updated:       true,
		Delta: agent.CoreMemoryDelta{
			BeforeChars:    120,
			AfterChars:     156,
			AddedCount:     2,
			RemovedCount:   1,
			KeptCount:      4,
			AddedPreview:   []string{"uses PST timestamps"},
			RemovedPreview: []string{"uses UTC timestamps"},
		},
	})
	if !strings.Contains(done, "dreaming done:") || !strings.Contains(done, "status: updated") {
		t.Fatalf("unexpected done format: %q", done)
	}
	if !strings.Contains(done, "changes: +2 -1 =4") {
		t.Fatalf("missing change summary in done format: %q", done)
	}
	if !strings.Contains(done, "chars: 120 -> 156") {
		t.Fatalf("missing char summary in done format: %q", done)
	}
	if !strings.Contains(done, "added:\n- uses PST timestamps") {
		t.Fatalf("missing added preview in done format: %q", done)
	}
	if !strings.Contains(done, "removed:\n- uses UTC timestamps") {
		t.Fatalf("missing removed preview in done format: %q", done)
	}
	if !strings.Contains(done, "duration_ms: 1234") {
		t.Fatalf("missing duration in done format: %q", done)
	}

	failed := formatCoreMemoryEvent(agent.CoreMemoryEvent{
		Phase:         agent.CoreMemoryEventFailure,
		UserTurnCount: 30,
		DurationMs:    50,
		Error:         "network timeout",
	})
	if !strings.Contains(failed, "dreaming failed:") {
		t.Fatalf("missing failure header: %q", failed)
	}
	if !strings.Contains(failed, "error: network timeout") {
		t.Fatalf("missing failure error body: %q", failed)
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
