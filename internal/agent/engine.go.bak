package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/ollama"
	"github.com/ParthSareen/OllamaClaw/internal/plugin"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
)

const systemPrompt = `You are OllamaClaw, a coding agent.
Use tools when needed. Be concise, accurate, and action-oriented.
When tool output is long, summarize key findings.
Never start, stop, or relaunch OllamaClaw itself from tools, and never modify launch lock files.
For self-debugging and telemetry, use read_logs when you need runtime traces.
Check ~/.ollamaclaw/workspace/notes.md at session start and when making notes to remember user preferences and context.`

type Engine struct {
	cfg           config.Config
	store         *db.Store
	client        *ollama.Client
	pluginManager *plugin.Manager
	builtinTools  []tools.Tool
}

type HandleResult struct {
	Session          db.Session
	AssistantContent string
	PromptTokens     int
	EvalTokens       int
	Compacted        bool
	ToolTrace        []ToolTraceEntry
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

func New(cfg config.Config, store *db.Store, client *ollama.Client, pm *plugin.Manager, cronCtrl tools.CronController) *Engine {
	builtin := tools.BuiltinTools(tools.BuiltinsConfig{
		ToolOutputMaxBytes: cfg.ToolOutputMaxBytes,
		BashTimeoutSec:     cfg.BashTimeoutSeconds,
		LogPath:            cfg.LogPath,
		Cron:               cronCtrl,
	}, client)
	return &Engine{cfg: cfg, store: store, client: client, pluginManager: pm, builtinTools: builtin}
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
	if err := e.store.InsertMessage(ctx, &db.Message{SessionID: sess.ID, Role: "user", Content: input}); err != nil {
		return HandleResult{}, err
	}

	model := sess.ModelOverride
	if strings.TrimSpace(model) == "" {
		model = e.cfg.DefaultModel
	}

	thinkEnabled, _ := e.IsSessionThink(ctx, transport, sessionKey)

	var lastReply string
	var promptTokens int
	var evalTokens int
	compacted := false
	toolTrace := []ToolTraceEntry{}
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
		resp, err := e.client.Chat(ctx, ollama.ChatRequest{Model: model, Messages: msgList, Tools: toolDefs, Stream: false, Think: thinkEnabled})
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

		if len(resp.Message.ToolCalls) == 0 && strings.TrimSpace(resp.Message.Content) != "" {
			lastReply = resp.Message.Content
		}

		justCompacted, err := e.maybeCompact(ctx, sess, model, resp.PromptEvalCount, thinkEnabled)
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

	return HandleResult{
		Session:          sess,
		AssistantContent: lastReply,
		PromptTokens:     promptTokens,
		EvalTokens:       evalTokens,
		Compacted:        compacted,
		ToolTrace:        toolTrace,
	}, nil
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
	v, ok, err := e.store.GetSetting(ctx, thinkSettingKey(transport, sessionKey))
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

func (e *Engine) SetSessionThink(ctx context.Context, transport, sessionKey string, enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	return e.store.SetSetting(ctx, thinkSettingKey(transport, sessionKey), val)
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
	messages := []ollama.ChatMessage{{Role: "system", Content: systemPrompt}}
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

func (e *Engine) maybeCompact(ctx context.Context, sess db.Session, model string, promptEvalCount int, thinkEnabled bool) (bool, error) {
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
	summaryResp, err := e.client.Chat(ctx, ollama.ChatRequest{Model: model, Messages: summaryPrompt, Stream: false, Think: thinkEnabled})
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

func truncateForTrace(v string, max int) string {
	if max <= 0 || len(v) <= max {
		return v
	}
	if max <= 3 {
		return v[:max]
	}
	return v[:max-3] + "..."
}
