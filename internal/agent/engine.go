package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/parth/ollamaclaw/internal/config"
	"github.com/parth/ollamaclaw/internal/db"
	"github.com/parth/ollamaclaw/internal/ollama"
	"github.com/parth/ollamaclaw/internal/plugin"
	"github.com/parth/ollamaclaw/internal/tools"
)

const baseSystemPrompt = `You are OllamaClaw, a coding agent.
Use tools when needed. Be concise, accurate, and action-oriented.
When tool output is long, summarize key findings.`

const cloudSystemAddendum = `Cloud model safety mode is enabled.
Assume prompts and tool outputs may be processed remotely.
Minimize sensitive data exposure:
- Prefer summaries over raw dumps.
- Share only the smallest snippet needed to complete the task.
- Never reveal secrets or tokens; redact sensitive values if encountered.
- For file and web content, avoid returning unrelated private data.`

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
}

func New(cfg config.Config, store *db.Store, client *ollama.Client, pm *plugin.Manager) *Engine {
	builtin := tools.BuiltinTools(tools.BuiltinsConfig{
		ToolOutputMaxBytes: cfg.ToolOutputMaxBytes,
		BashTimeoutSec:     cfg.BashTimeoutSeconds,
	}, client)
	return &Engine{cfg: cfg, store: store, client: client, pluginManager: pm, builtinTools: builtin}
}

func (e *Engine) HandleText(ctx context.Context, transport, sessionKey, input string) (HandleResult, error) {
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

	var lastReply string
	var promptTokens int
	var evalTokens int
	compacted := false

	for i := 0; i < 12; i++ {
		combined, err := e.combinedTools(ctx)
		if err != nil {
			return HandleResult{}, err
		}
		toolDefs := toOllamaTools(combined)
		msgList, err := e.activePromptMessages(ctx, sess.ID, model)
		if err != nil {
			return HandleResult{}, err
		}
		resp, err := e.client.Chat(ctx, ollama.ChatRequest{Model: model, Messages: msgList, Tools: toolDefs, Stream: false})
		if err != nil {
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

		if strings.TrimSpace(resp.Message.Content) != "" {
			lastReply = resp.Message.Content
		}

		justCompacted, err := e.maybeCompact(ctx, sess, model, resp.PromptEvalCount)
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
			name := call.Function.Name
			args := call.Function.Arguments
			result := map[string]interface{}{}
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
					result["error"] = err.Error()
				} else {
					result = r
				}
			} else {
				result["error"] = fmt.Sprintf("tool %s not found", name)
			}
			payload, _ := json.Marshal(result)
			if len(payload) > e.cfg.ToolOutputMaxBytes {
				payload = payload[:e.cfg.ToolOutputMaxBytes]
			}
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
	}, nil
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

func (e *Engine) activePromptMessages(ctx context.Context, sessionID, model string) ([]ollama.ChatMessage, error) {
	messages := []ollama.ChatMessage{{Role: "system", Content: systemPromptForModel(model)}}
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

func (e *Engine) maybeCompact(ctx context.Context, sess db.Session, model string, promptEvalCount int) (bool, error) {
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
		{Role: "system", Content: compactionPromptForModel(model)},
		{Role: "user", Content: "Previous summary:\n" + latestSummary + "\n\nMessages to summarize:\n" + string(b)},
	}
	summaryResp, err := e.client.Chat(ctx, ollama.ChatRequest{Model: model, Messages: summaryPrompt, Stream: false})
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

func systemPromptForModel(model string) string {
	if usesCloudModel(model) {
		return baseSystemPrompt + "\n\n" + cloudSystemAddendum
	}
	return baseSystemPrompt
}

func compactionPromptForModel(model string) string {
	prompt := "Summarize the archived conversation for future continuation. Include decisions, constraints, file/task state, and unresolved items."
	if usesCloudModel(model) {
		prompt += " Do not include secrets, tokens, or unnecessary private content."
	}
	return prompt
}

func usesCloudModel(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasSuffix(m, ":cloud") || strings.HasSuffix(m, "-cloud") || strings.Contains(m, ":cloud-")
}
