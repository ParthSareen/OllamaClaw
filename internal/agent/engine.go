package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/ollama"
	"github.com/ParthSareen/OllamaClaw/internal/plugin"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
)

const (
	coreMemoriesTurnInterval = 10
	coreMemoriesMaxMessages  = 40
	coreMemoriesTimeout      = 30 * time.Second
	coreMemoriesMaxChars     = 4000
	coreMemoriesStartMarker  = "<!-- OLLAMACLAW_CORE_MEMORIES_START -->"
	coreMemoriesEndMarker    = "<!-- OLLAMACLAW_CORE_MEMORIES_END -->"
)

const defaultSystemPrompt = `You are OllamaClaw, a fast coding copilot with startup energy.

Tone and style:
- Be crisp, optimistic, and a little witty (never goofy).
- Keep responses concise and high-signal unless the user asks for depth.
- During incidents/debugging, prioritize clarity over humor.
- Add brief playful color when it helps morale, but keep technical guidance precise.
- Celebrate progress with short confidence-boosting lines when work lands cleanly.

Response format:
- Default to: Plan -> Action -> Result.
- When you use tools, include one short transparency line naming the tool and why it was used.

Execution behavior:
- Prefer solving over narrating.
- Use tools whenever they reduce guesswork, improve speed, or increase correctness.
- For prompt tuning, use managed system_prompt tools instead of directly editing prompt files.
- Never fabricate tool results, file contents, command outcomes, or links.
- If tool output is long, summarize key findings first, then include critical details.
- If blocked, state the blocker plainly and give the best next action immediately.

Runtime safety:
- Never start, stop, or relaunch OllamaClaw itself from tools.
- Never modify launch lock files.
- For self-debugging and telemetry, use read_logs when you need runtime traces.

CRON behavior:
- When a cron job includes prefetched command outputs, treat them as primary run data.
- Reuse prefetched outputs when sufficient; call extra tools only for missing or stale data.
- Prefer stable read-only commands for recurring cron tasks so they can be auto-prefetched.
- For CI/PR checks: run gh pr view <PR_NUM> for current status.
- For time-sensitive tasks: always query the source; do not reuse stale info.
- Cron prompts may be brief; infer and execute the needed tool calls.
- Report only relevant results.

Timezone policy:
- Treat all scheduling and time-based operations in America/Los_Angeles (PST/PDT).
- Convert timezone-based outputs into America/Los_Angeles before presenting times.`

type Engine struct {
	cfg            config.Config
	store          *db.Store
	client         *ollama.Client
	pluginManager  *plugin.Manager
	builtinTools   []tools.Tool
	memoryMu       sync.Mutex
	memoryInFlight map[string]struct{}
}

type HandleResult struct {
	Session          db.Session
	AssistantContent string
	PromptTokens     int
	EvalTokens       int
	Compacted        bool
	ToolTrace        []ToolTraceEntry
	ThinkingTrace    []ThinkingTraceEntry
}

type HandleOptions struct {
	OnToolEvent func(ToolEvent)
}

type ToolEventPhase string

const (
	ToolEventStart  ToolEventPhase = "start"
	ToolEventFinish ToolEventPhase = "finish"
)

type ToolEvent struct {
	Phase      ToolEventPhase
	Index      int
	Name       string
	ArgsJSON   string
	ResultJSON string
	Error      string
	DurationMs int64
}

type ToolTraceEntry struct {
	Name       string
	ArgsJSON   string
	ResultJSON string
	Error      string
	DurationMs int64
}

type ThinkingTraceEntry struct {
	Step          int
	Thinking      string
	ToolCallCount int
}

func New(cfg config.Config, store *db.Store, client *ollama.Client, pm *plugin.Manager, cronCtrl tools.CronController) *Engine {
	builtin := tools.BuiltinTools(tools.BuiltinsConfig{
		ToolOutputMaxBytes: cfg.ToolOutputMaxBytes,
		BashTimeoutSec:     cfg.BashTimeoutSeconds,
		LogPath:            cfg.LogPath,
		Cron:               cronCtrl,
	}, client)
	return &Engine{
		cfg:            cfg,
		store:          store,
		client:         client,
		pluginManager:  pm,
		builtinTools:   builtin,
		memoryInFlight: map[string]struct{}{},
	}
}

func (e *Engine) HandleText(ctx context.Context, transport, sessionKey, input string) (HandleResult, error) {
	return e.HandleTextWithOptions(ctx, transport, sessionKey, input, HandleOptions{})
}

func (e *Engine) HandleTextWithOptions(ctx context.Context, transport, sessionKey, input string, opts HandleOptions) (HandleResult, error) {
	ctx = tools.WithSessionInfo(ctx, transport, sessionKey)
	sess, err := e.store.GetOrCreateActiveSession(ctx, transport, sessionKey, e.cfg.DefaultModel)
	if err != nil {
		return HandleResult{}, err
	}
	if prefetched, ok := tools.PrefetchedBashResultsFromContext(ctx); ok && len(prefetched) > 0 {
		if err := e.injectPrefetchedBashContext(ctx, sess.ID, prefetched); err != nil {
			return HandleResult{}, err
		}
	}
	if err := e.store.InsertMessage(ctx, &db.Message{SessionID: sess.ID, Role: "user", Content: input}); err != nil {
		return HandleResult{}, err
	}

	model := sess.ModelOverride
	if strings.TrimSpace(model) == "" {
		model = e.cfg.DefaultModel
	}

	thinkSetting, _ := e.SessionThinkValue(ctx, transport, sessionKey)
	thinkParam := thinkSettingToAPIValue(thinkSetting)

	var lastReply string
	var promptTokens int
	var evalTokens int
	compacted := false
	toolTrace := []ToolTraceEntry{}
	thinkingTrace := []ThinkingTraceEntry{}
	toolCallIndex := 0

	for i := 0; i < 12; i++ {
		if err := ctx.Err(); err != nil {
			return HandleResult{}, err
		}
		combined, err := e.combinedTools(ctx)
		if err != nil {
			return HandleResult{}, err
		}
		toolDefs := toOllamaTools(combined)
		msgList, err := e.activePromptMessages(ctx, sess.ID)
		if err != nil {
			return HandleResult{}, err
		}
		resp, err := e.client.Chat(ctx, ollama.ChatRequest{Model: model, Messages: msgList, Tools: toolDefs, Stream: false, Think: thinkParam})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return HandleResult{}, cerr
			}
			return HandleResult{}, err
		}
		promptTokens = resp.PromptEvalCount
		evalTokens = resp.EvalCount
		_ = e.store.IncrementSessionTokens(ctx, sess.ID, resp.PromptEvalCount, resp.EvalCount)

		toolCallsJSON := ""
		if len(resp.Message.ToolCalls) > 0 {
			b, _ := json.Marshal(resp.Message.ToolCalls)
			toolCallsJSON = string(b)
		}
		assistantMsg := db.Message{
			SessionID:       sess.ID,
			Role:            "assistant",
			Content:         resp.Message.Content,
			Thinking:        resp.Message.Thinking,
			ToolCallsJSON:   toolCallsJSON,
			PromptEvalCount: resp.PromptEvalCount,
			EvalCount:       resp.EvalCount,
		}
		if err := e.store.InsertMessage(ctx, &assistantMsg); err != nil {
			return HandleResult{}, err
		}
		if strings.TrimSpace(resp.Message.Thinking) != "" {
			thinkingTrace = append(thinkingTrace, ThinkingTraceEntry{
				Step:          i + 1,
				Thinking:      resp.Message.Thinking,
				ToolCallCount: len(resp.Message.ToolCalls),
			})
		}

		if len(resp.Message.ToolCalls) == 0 && strings.TrimSpace(resp.Message.Content) != "" {
			lastReply = resp.Message.Content
		}

		justCompacted, err := e.maybeCompact(ctx, sess, model, resp.PromptEvalCount, thinkParam)
		if err != nil {
			return HandleResult{}, err
		}
		if justCompacted {
			compacted = true
		}

		if len(resp.Message.ToolCalls) == 0 {
			break
		}

		toolMap := tools.ToolMap(combined)
		for _, call := range resp.Message.ToolCalls {
			if err := ctx.Err(); err != nil {
				return HandleResult{}, err
			}
			toolCallIndex++
			name := call.Function.Name
			args := call.Function.Arguments
			result := map[string]interface{}{}
			trace := ToolTraceEntry{Name: name, ArgsJSON: mustJSON(args)}
			e.emitToolEvent(opts.OnToolEvent, ToolEvent{
				Phase:    ToolEventStart,
				Index:    toolCallIndex,
				Name:     name,
				ArgsJSON: trace.ArgsJSON,
			})
			startedAt := time.Now()
			if t, ok := toolMap[name]; ok {
				r, err := func() (map[string]interface{}, error) {
					if t.TimeoutSec <= 0 {
						return t.Execute(ctx, args)
					}
					ctxTool, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutSec)*time.Second)
					defer cancel()
					return t.Execute(ctxTool, args)
				}()
				if err != nil {
					if cerr := ctx.Err(); cerr != nil {
						trace.DurationMs = time.Since(startedAt).Milliseconds()
						trace.Error = cerr.Error()
						e.emitToolEvent(opts.OnToolEvent, ToolEvent{
							Phase:      ToolEventFinish,
							Index:      toolCallIndex,
							Name:       name,
							ArgsJSON:   trace.ArgsJSON,
							Error:      trace.Error,
							DurationMs: trace.DurationMs,
						})
						return HandleResult{}, cerr
					}
					errMsg := err.Error()
					result["error"] = errMsg
					trace.Error = errMsg
				} else {
					result = r
				}
			} else {
				errMsg := fmt.Sprintf("tool %s not found", name)
				result["error"] = errMsg
				trace.Error = errMsg
			}
			trace.DurationMs = time.Since(startedAt).Milliseconds()
			payload, _ := json.Marshal(result)
			if len(payload) > e.cfg.ToolOutputMaxBytes {
				payload = payload[:e.cfg.ToolOutputMaxBytes]
			}
			trace.ResultJSON = truncateForTrace(string(payload), 2000)
			e.emitToolEvent(opts.OnToolEvent, ToolEvent{
				Phase:      ToolEventFinish,
				Index:      toolCallIndex,
				Name:       name,
				ArgsJSON:   trace.ArgsJSON,
				ResultJSON: trace.ResultJSON,
				Error:      trace.Error,
				DurationMs: trace.DurationMs,
			})
			toolTrace = append(toolTrace, trace)
			if err := e.store.InsertMessage(ctx, &db.Message{
				SessionID:    sess.ID,
				Role:         "tool",
				ToolName:     name,
				ToolArgsJSON: mustJSON(args),
				Content:      string(payload),
			}); err != nil {
				return HandleResult{}, err
			}
		}
	}

	result := HandleResult{
		Session:          sess,
		AssistantContent: lastReply,
		PromptTokens:     promptTokens,
		EvalTokens:       evalTokens,
		Compacted:        compacted,
		ToolTrace:        toolTrace,
		ThinkingTrace:    thinkingTrace,
	}
	e.maybeScheduleCoreMemoriesRefresh(sess, model)
	return result, nil
}

func (e *Engine) emitToolEvent(cb func(ToolEvent), ev ToolEvent) {
	if cb == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	cb(ev)
}

func (e *Engine) injectPrefetchedBashContext(ctx context.Context, sessionID string, prefetched []tools.PrefetchedBashResult) error {
	for i, p := range prefetched {
		call := []ollama.ToolCall{
			{
				Function: ollama.ToolCallFunction{
					Name:      "bash",
					Arguments: map[string]interface{}{"command": p.Command},
				},
			},
		}
		callJSON, _ := json.Marshal(call)
		if err := e.store.InsertMessage(ctx, &db.Message{
			SessionID:     sessionID,
			Role:          "assistant",
			Content:       fmt.Sprintf("Host prefetch step %d/%d", i+1, len(prefetched)),
			ToolCallsJSON: string(callJSON),
		}); err != nil {
			return err
		}
		payload := map[string]interface{}{
			"prefetched":  true,
			"fetched_at":  p.FetchedAt,
			"exit_code":   p.ExitCode,
			"stdout":      p.Stdout,
			"stderr":      p.Stderr,
			"duration_ms": p.DurationMs,
		}
		b, _ := json.Marshal(payload)
		if err := e.store.InsertMessage(ctx, &db.Message{
			SessionID:    sessionID,
			Role:         "tool",
			ToolName:     "bash",
			ToolArgsJSON: mustJSON(map[string]interface{}{"command": p.Command}),
			Content:      string(b),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ListTools(ctx context.Context) ([]tools.Tool, error) {
	return e.combinedTools(ctx)
}

func (e *Engine) GetOrCreateSession(ctx context.Context, transport, sessionKey string) (db.Session, error) {
	return e.store.GetOrCreateActiveSession(ctx, transport, sessionKey, e.cfg.DefaultModel)
}

func (e *Engine) SetSessionModel(ctx context.Context, sessionID, model string) error {
	return e.store.SetSessionModel(ctx, sessionID, model)
}

func (e *Engine) ResetSession(ctx context.Context, transport, sessionKey string) (db.Session, error) {
	return e.store.ResetSession(ctx, transport, sessionKey, e.cfg.DefaultModel)
}

func (e *Engine) IsSessionVerbose(ctx context.Context, transport, sessionKey string) (bool, error) {
	v, ok, err := e.store.GetSetting(ctx, verboseSettingKey(transport, sessionKey))
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func (e *Engine) SetSessionVerbose(ctx context.Context, transport, sessionKey string, enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	return e.store.SetSetting(ctx, verboseSettingKey(transport, sessionKey), val)
}

func (e *Engine) IsSessionShowTools(ctx context.Context, transport, sessionKey string) (bool, error) {
	v, ok, err := e.store.GetSetting(ctx, showToolsSettingKey(transport, sessionKey))
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func (e *Engine) SetSessionShowTools(ctx context.Context, transport, sessionKey string, enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	return e.store.SetSetting(ctx, showToolsSettingKey(transport, sessionKey), val)
}

func (e *Engine) IsSessionThink(ctx context.Context, transport, sessionKey string) (bool, error) {
	value, err := e.SessionThinkValue(ctx, transport, sessionKey)
	if err != nil {
		return false, err
	}
	switch value {
	case "on", "low", "medium", "high":
		return true, nil
	default:
		return false, nil
	}
}

func (e *Engine) SetSessionThink(ctx context.Context, transport, sessionKey string, enabled bool) error {
	if enabled {
		return e.SetSessionThinkValue(ctx, transport, sessionKey, "on")
	}
	return e.SetSessionThinkValue(ctx, transport, sessionKey, "off")
}

func (e *Engine) SessionThinkValue(ctx context.Context, transport, sessionKey string) (string, error) {
	v, ok, err := e.store.GetSetting(ctx, thinkSettingKey(transport, sessionKey))
	if err != nil {
		return "", err
	}
	if !ok {
		return "off", nil
	}
	normalized, valid := normalizeThinkSetting(v)
	if !valid {
		return "off", nil
	}
	return normalized, nil
}

func (e *Engine) SetSessionThinkValue(ctx context.Context, transport, sessionKey, value string) error {
	normalized, valid := normalizeThinkSetting(value)
	if !valid {
		return fmt.Errorf("invalid think value: %q", value)
	}
	return e.store.SetSetting(ctx, thinkSettingKey(transport, sessionKey), normalized)
}

func (e *Engine) combinedTools(ctx context.Context) ([]tools.Tool, error) {
	pluginTools, err := e.pluginManager.LoadEnabledTools(ctx)
	if err != nil {
		return nil, err
	}
	all := append([]tools.Tool{}, e.builtinTools...)
	seen := map[string]bool{}
	for _, t := range all {
		seen[t.Name] = true
	}
	for _, t := range pluginTools {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		all = append(all, t)
	}
	return all, nil
}

func (e *Engine) activePromptMessages(ctx context.Context, sessionID string) ([]ollama.ChatMessage, error) {
	messages := []ollama.ChatMessage{{Role: "system", Content: e.runtimeSystemPrompt()}}
	if core := strings.TrimSpace(e.runtimeCoreMemories()); core != "" {
		messages = append(messages, ollama.ChatMessage{Role: "system", Content: "Core memories:\n" + core})
	}
	summary, ok, err := e.store.LatestCompactionSummary(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if ok && strings.TrimSpace(summary) != "" {
		messages = append(messages, ollama.ChatMessage{Role: "system", Content: "Conversation summary:\n" + summary})
	}
	rows, err := e.store.ListMessages(ctx, sessionID, false)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		msg := ollama.ChatMessage{Role: row.Role, Content: row.Content, Thinking: row.Thinking, ToolName: row.ToolName}
		if strings.TrimSpace(row.ToolCallsJSON) != "" {
			var tc []ollama.ToolCall
			if err := json.Unmarshal([]byte(row.ToolCallsJSON), &tc); err == nil {
				msg.ToolCalls = tc
			}
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

func (e *Engine) runtimeSystemPrompt() string {
	overlay := e.runtimeSystemPromptOverlay()
	path, err := config.SystemPromptPath()
	if err != nil {
		return composeSystemPrompt(defaultSystemPrompt, overlay)
	}
	if b, err := os.ReadFile(path); err == nil {
		text := strings.TrimSpace(string(b))
		if text != "" {
			return composeSystemPrompt(string(b), overlay)
		}
		return composeSystemPrompt(defaultSystemPrompt, overlay)
	} else if !errors.Is(err, os.ErrNotExist) {
		return composeSystemPrompt(defaultSystemPrompt, overlay)
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(defaultSystemPrompt), 0o600)
	return composeSystemPrompt(defaultSystemPrompt, overlay)
}

func withTimezonePolicyPrompt(base string) string {
	text := strings.TrimSpace(base)
	if text == "" {
		text = defaultSystemPrompt
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "america/los_angeles") || strings.Contains(lower, "pst/pdt") {
		return text
	}
	addendum := "\n\nTimezone policy:\n- Treat all scheduling and time-based operations in America/Los_Angeles (PST/PDT).\n- Convert timezone-based outputs into America/Los_Angeles before presenting times."
	return text + addendum
}

func composeSystemPrompt(base, overlay string) string {
	text := withTimezonePolicyPrompt(base)
	overlay = strings.TrimSpace(overlay)
	if overlay == "" {
		return text
	}
	return text + "\n\nManaged Prompt Overlay:\n" + overlay
}

func (e *Engine) runtimeSystemPromptOverlay() string {
	path, err := config.SystemPromptOverlayPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (e *Engine) runtimeCoreMemories() string {
	path, err := config.CoreMemoriesPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return clampToMaxChars(extractManagedCoreMemories(string(b)), coreMemoriesMaxChars)
}

func (e *Engine) maybeScheduleCoreMemoriesRefresh(sess db.Session, model string) {
	userTurnCount, err := e.store.CountMessagesByRole(context.Background(), sess.ID, "user")
	if err != nil || userTurnCount <= 0 || userTurnCount%coreMemoriesTurnInterval != 0 {
		return
	}
	settingKey := coreMemoriesLastTurnSettingKey(sess.ID)
	lastTurn := 0
	if v, ok, err := e.store.GetSetting(context.Background(), settingKey); err == nil && ok {
		if n, convErr := strconv.Atoi(strings.TrimSpace(v)); convErr == nil {
			lastTurn = n
		}
	}
	if lastTurn >= userTurnCount {
		return
	}

	e.memoryMu.Lock()
	if _, exists := e.memoryInFlight[sess.ID]; exists {
		e.memoryMu.Unlock()
		return
	}
	e.memoryInFlight[sess.ID] = struct{}{}
	e.memoryMu.Unlock()

	go func() {
		defer func() {
			e.memoryMu.Lock()
			delete(e.memoryInFlight, sess.ID)
			e.memoryMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), coreMemoriesTimeout)
		defer cancel()
		if err := e.refreshCoreMemories(ctx, sess, model); err != nil {
			return
		}
		_ = e.store.SetSetting(ctx, settingKey, strconv.Itoa(userTurnCount))
	}()
}

func (e *Engine) refreshCoreMemories(ctx context.Context, sess db.Session, model string) error {
	rows, err := e.store.ListMessages(ctx, sess.ID, false)
	if err != nil {
		return err
	}
	conversation := compactConversationForCoreMemory(rows, coreMemoriesMaxMessages)
	if strings.TrimSpace(conversation) == "" {
		return nil
	}
	path, err := config.CoreMemoriesPath()
	if err != nil {
		return err
	}
	existing := ""
	if b, readErr := os.ReadFile(path); readErr == nil {
		existing = string(b)
	}
	existingCore := clampToMaxChars(extractManagedCoreMemories(existing), coreMemoriesMaxChars)
	memModel := strings.TrimSpace(model)
	if memModel == "" {
		memModel = e.cfg.DefaultModel
	}
	req := []ollama.ChatMessage{
		{
			Role:    "system",
			Content: "Update the assistant's durable core memories from conversation logs. Keep only stable preferences, communication style, workflows, constraints, and long-term context. Exclude ephemeral details. Output concise Markdown bullets only. Keep total output at or below 4000 characters.",
		},
		{
			Role:    "user",
			Content: fmt.Sprintf("Existing core memories:\n%s\n\nRecent conversation:\n%s\n\nReturn only updated core memories as Markdown bullets (max 20 bullets, max 4000 characters).", existingCore, conversation),
		},
	}
	resp, err := e.client.Chat(ctx, ollama.ChatRequest{
		Model:    memModel,
		Messages: req,
		Stream:   false,
		Think:    false,
	})
	if err != nil {
		return err
	}
	updatedCore := clampToMaxChars(strings.TrimSpace(resp.Message.Content), coreMemoriesMaxChars)
	if updatedCore == "" {
		return nil
	}
	merged := upsertManagedCoreMemories(existing, updatedCore)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(merged), 0o600)
}

func compactConversationForCoreMemory(rows []db.Message, maxMessages int) string {
	if maxMessages <= 0 {
		maxMessages = coreMemoriesMaxMessages
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		switch row.Role {
		case "user", "assistant":
			content := strings.TrimSpace(row.Content)
			if content == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s: %s", row.Role, content))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > maxMessages {
		lines = lines[len(lines)-maxMessages:]
	}
	return strings.Join(lines, "\n")
}

func extractManagedCoreMemories(existing string) string {
	start := strings.Index(existing, coreMemoriesStartMarker)
	end := strings.Index(existing, coreMemoriesEndMarker)
	if start < 0 || end < 0 || end <= start {
		return strings.TrimSpace(existing)
	}
	content := existing[start+len(coreMemoriesStartMarker) : end]
	return strings.TrimSpace(content)
}

func upsertManagedCoreMemories(existing, core string) string {
	core = strings.TrimSpace(core)
	managed := coreMemoriesStartMarker + "\n" + core + "\n" + coreMemoriesEndMarker
	start := strings.Index(existing, coreMemoriesStartMarker)
	end := strings.Index(existing, coreMemoriesEndMarker)
	if start >= 0 && end > start {
		end += len(coreMemoriesEndMarker)
		updated := strings.TrimRight(existing[:start], "\n")
		suffix := strings.TrimLeft(existing[end:], "\n")
		if updated == "" && suffix == "" {
			return managed + "\n"
		}
		if suffix == "" {
			return updated + "\n\n" + managed + "\n"
		}
		if updated == "" {
			return managed + "\n\n" + suffix
		}
		return updated + "\n\n" + managed + "\n\n" + suffix
	}
	prefix := strings.TrimSpace(existing)
	if prefix == "" {
		return managed + "\n"
	}
	return prefix + "\n\n" + managed + "\n"
}

func coreMemoriesLastTurnSettingKey(sessionID string) string {
	return "core_memories_last_turn:" + strings.TrimSpace(sessionID)
}

func (e *Engine) maybeCompact(ctx context.Context, sess db.Session, model string, promptEvalCount int, thinkParam interface{}) (bool, error) {
	thresholdTokens := int(float64(e.cfg.ContextWindowTokens) * e.cfg.CompactionThreshold)
	if promptEvalCount < thresholdTokens {
		return false, nil
	}
	rows, err := e.store.ListMessages(ctx, sess.ID, false)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	userIndices := []int{}
	for i, m := range rows {
		if m.Role == "user" {
			userIndices = append(userIndices, i)
		}
	}
	if len(userIndices) <= e.cfg.KeepRecentTurns {
		return false, nil
	}
	keepStart := userIndices[len(userIndices)-e.cfg.KeepRecentTurns]
	toSummarize := rows[:keepStart]
	if len(toSummarize) == 0 {
		return false, nil
	}
	latestSummary, _, err := e.store.LatestCompactionSummary(ctx, sess.ID)
	if err != nil {
		return false, err
	}

	type compactMessage struct {
		Role      string `json:"role"`
		Content   string `json:"content"`
		ToolName  string `json:"tool_name,omitempty"`
		Thinking  string `json:"thinking,omitempty"`
		ToolCalls string `json:"tool_calls,omitempty"`
	}
	payload := make([]compactMessage, 0, len(toSummarize))
	ids := make([]int64, 0, len(toSummarize))
	for _, row := range toSummarize {
		payload = append(payload, compactMessage{Role: row.Role, Content: row.Content, ToolName: row.ToolName, Thinking: row.Thinking, ToolCalls: row.ToolCallsJSON})
		ids = append(ids, row.ID)
	}
	b, _ := json.Marshal(payload)
	summaryPrompt := []ollama.ChatMessage{
		{Role: "system", Content: "Summarize the archived conversation for future continuation. Include decisions, constraints, file/task state, and unresolved items."},
		{Role: "user", Content: "Previous summary:\n" + latestSummary + "\n\nMessages to summarize:\n" + string(b)},
	}
	summaryResp, err := e.client.Chat(ctx, ollama.ChatRequest{Model: model, Messages: summaryPrompt, Stream: false, Think: thinkParam})
	if err != nil {
		return false, err
	}
	summary := strings.TrimSpace(summaryResp.Message.Content)
	if summary == "" {
		summary = "Summary unavailable; previous context compacted."
	}
	if err := e.store.InsertCompaction(ctx, db.Compaction{
		SessionID:         sess.ID,
		Summary:           summary,
		FirstKeptMessage:  rows[keepStart].ID,
		ArchivedBeforeSeq: rows[keepStart].Seq,
	}); err != nil {
		return false, err
	}
	if err := e.store.ArchiveMessagesByIDs(ctx, sess.ID, ids); err != nil {
		return false, err
	}
	if err := e.store.IncrementCompactions(ctx, sess.ID); err != nil {
		return false, err
	}
	return true, nil
}

func toOllamaTools(in []tools.Tool) []ollama.ToolDefinition {
	out := make([]ollama.ToolDefinition, 0, len(in))
	for _, t := range in {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, ollama.ToolDefinition{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	return out
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func verboseSettingKey(transport, sessionKey string) string {
	return fmt.Sprintf("session_verbose:%s:%s", transport, sessionKey)
}

func showToolsSettingKey(transport, sessionKey string) string {
	return fmt.Sprintf("session_show_tools:%s:%s", transport, sessionKey)
}

func thinkSettingKey(transport, sessionKey string) string {
	return fmt.Sprintf("session_think:%s:%s", transport, sessionKey)
}

func normalizeThinkSetting(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes":
		return "on", true
	case "0", "false", "off", "no":
		return "off", true
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(raw)), true
	case "default", "auto":
		return "default", true
	default:
		return "", false
	}
}

func thinkSettingToAPIValue(value string) interface{} {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on":
		return true
	case "off":
		return false
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(value))
	case "default":
		return nil
	default:
		return false
	}
}

func truncateForTrace(v string, max int) string {
	if max <= 0 || len(v) <= max {
		return v
	}
	if max <= 3 {
		return v[:max]
	}
	return v[:max-3] + "..."
}

func clampToMaxChars(s string, maxChars int) string {
	text := strings.TrimSpace(s)
	if maxChars <= 0 || text == "" {
		return text
	}
	r := []rune(text)
	if len(r) <= maxChars {
		return text
	}
	clamped := strings.TrimSpace(string(r[:maxChars]))
	return clamped
}
