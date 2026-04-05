package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/ollama"
	"github.com/ParthSareen/OllamaClaw/internal/plugin"
)

func TestHandleTextWithReadFileToolTrace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	filePath := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(filePath, []byte("hello from file"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		callCount++

		var resp ollama.ChatResponse
		if callCount == 1 {
			resp = ollama.ChatResponse{
				Message: ollama.ChatMessage{
					Role: "assistant",
					ToolCalls: []ollama.ToolCall{
						{
							Function: ollama.ToolCallFunction{
								Name:      "read_file",
								Arguments: map[string]interface{}{"path": filePath},
							},
						},
					},
				},
				PromptEvalCount: 20,
				EvalCount:       3,
				Done:            true,
			}
		} else if callCount == 2 {
			foundTool := false
			for _, m := range req.Messages {
				if m.Role == "tool" && m.ToolName == "read_file" {
					foundTool = true
					break
				}
			}
			if !foundTool {
				t.Fatalf("expected tool message for read_file in second chat request")
			}
			resp = ollama.ChatResponse{
				Message:         ollama.ChatMessage{Role: "assistant", Content: "done"},
				PromptEvalCount: 22,
				EvalCount:       4,
				Done:            true,
			}
		} else {
			t.Fatalf("unexpected extra chat request: %d", callCount)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	events := make([]ToolEvent, 0, 2)
	res, err := engine.HandleTextWithOptions(ctx, "repl", "default", "read that file", HandleOptions{
		OnToolEvent: func(ev ToolEvent) {
			events = append(events, ev)
		},
	})
	if err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if res.AssistantContent != "done" {
		t.Fatalf("expected final assistant content, got %q", res.AssistantContent)
	}
	if len(res.ToolTrace) != 1 {
		t.Fatalf("expected 1 tool trace entry, got %d", len(res.ToolTrace))
	}
	trace := res.ToolTrace[0]
	if trace.Name != "read_file" {
		t.Fatalf("unexpected tool name %q", trace.Name)
	}
	if trace.Error != "" {
		t.Fatalf("expected no tool error, got %q", trace.Error)
	}
	if !strings.Contains(trace.ResultJSON, "hello from file") {
		t.Fatalf("expected tool result to include file content, got %q", trace.ResultJSON)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 tool events, got %d", len(events))
	}
	if events[0].Phase != ToolEventStart || events[0].Name != "read_file" {
		t.Fatalf("unexpected first event: %+v", events[0])
	}
	if events[1].Phase != ToolEventFinish || events[1].Name != "read_file" {
		t.Fatalf("unexpected second event: %+v", events[1])
	}
	if strings.TrimSpace(events[1].ResultJSON) == "" {
		t.Fatalf("expected finish event to include result payload")
	}
	if callCount != 2 {
		t.Fatalf("expected 2 chat calls, got %d", callCount)
	}
}

func TestHandleTextUnknownToolTraceError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = req
		callCount++
		var resp ollama.ChatResponse
		if callCount == 1 {
			resp = ollama.ChatResponse{
				Message: ollama.ChatMessage{
					Role: "assistant",
					ToolCalls: []ollama.ToolCall{
						{
							Function: ollama.ToolCallFunction{
								Name:      "does_not_exist",
								Arguments: map[string]interface{}{"x": 1},
							},
						},
					},
				},
				PromptEvalCount: 10,
				EvalCount:       1,
			}
		} else {
			resp = ollama.ChatResponse{
				Message:         ollama.ChatMessage{Role: "assistant", Content: "ok"},
				PromptEvalCount: 11,
				EvalCount:       1,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	res, err := engine.HandleText(ctx, "repl", "default", "trigger unknown tool")
	if err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if res.AssistantContent != "ok" {
		t.Fatalf("expected final assistant content ok, got %q", res.AssistantContent)
	}
	if len(res.ToolTrace) != 1 {
		t.Fatalf("expected 1 tool trace entry, got %d", len(res.ToolTrace))
	}
	if !strings.Contains(res.ToolTrace[0].Error, "not found") {
		t.Fatalf("expected not found error in tool trace, got %q", res.ToolTrace[0].Error)
	}
}

func TestHandleTextDoesNotPromoteToolStepContentToFinal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	filePath := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var resp ollama.ChatResponse
		if callCount == 1 {
			resp = ollama.ChatResponse{
				Message: ollama.ChatMessage{
					Role:    "assistant",
					Content: "internal reasoning preamble that should not be final output",
					ToolCalls: []ollama.ToolCall{
						{
							Function: ollama.ToolCallFunction{
								Name:      "read_file",
								Arguments: map[string]interface{}{"path": filePath},
							},
						},
					},
				},
				PromptEvalCount: 14,
				EvalCount:       2,
			}
		} else {
			resp = ollama.ChatResponse{
				Message:         ollama.ChatMessage{Role: "assistant", Content: ""},
				PromptEvalCount: 16,
				EvalCount:       2,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	res, err := engine.HandleText(ctx, "repl", "default", "read that file")
	if err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if strings.TrimSpace(res.AssistantContent) != "" {
		t.Fatalf("expected empty final assistant content, got %q", res.AssistantContent)
	}
	if len(res.ToolTrace) != 1 {
		t.Fatalf("expected one tool trace entry, got %d", len(res.ToolTrace))
	}
}

func TestSessionVerboseRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	engine, store := newTestEngine(t, "http://127.0.0.1:65535")
	defer store.Close()

	enabled, err := engine.IsSessionVerbose(ctx, "repl", "default")
	if err != nil {
		t.Fatalf("IsSessionVerbose error: %v", err)
	}
	if enabled {
		t.Fatalf("expected default verbose=false")
	}

	if err := engine.SetSessionVerbose(ctx, "repl", "default", true); err != nil {
		t.Fatalf("SetSessionVerbose(true) error: %v", err)
	}
	enabled, err = engine.IsSessionVerbose(ctx, "repl", "default")
	if err != nil {
		t.Fatalf("IsSessionVerbose error: %v", err)
	}
	if !enabled {
		t.Fatalf("expected verbose=true after setting")
	}

	if err := engine.SetSessionVerbose(ctx, "repl", "default", false); err != nil {
		t.Fatalf("SetSessionVerbose(false) error: %v", err)
	}
	enabled, err = engine.IsSessionVerbose(ctx, "repl", "default")
	if err != nil {
		t.Fatalf("IsSessionVerbose error: %v", err)
	}
	if enabled {
		t.Fatalf("expected verbose=false after unsetting")
	}
}

func TestSessionShowToolsRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	engine, store := newTestEngine(t, "http://127.0.0.1:65535")
	defer store.Close()

	enabled, err := engine.IsSessionShowTools(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("IsSessionShowTools error: %v", err)
	}
	if enabled {
		t.Fatalf("expected default show_tools=false")
	}

	if err := engine.SetSessionShowTools(ctx, "telegram", "123", true); err != nil {
		t.Fatalf("SetSessionShowTools(true) error: %v", err)
	}
	enabled, err = engine.IsSessionShowTools(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("IsSessionShowTools error: %v", err)
	}
	if !enabled {
		t.Fatalf("expected show_tools=true after setting")
	}

	if err := engine.SetSessionShowTools(ctx, "telegram", "123", false); err != nil {
		t.Fatalf("SetSessionShowTools(false) error: %v", err)
	}
	enabled, err = engine.IsSessionShowTools(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("IsSessionShowTools error: %v", err)
	}
	if enabled {
		t.Fatalf("expected show_tools=false after unsetting")
	}
}

func TestHandleTextUsesSystemPromptFromHomeFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()

	promptPath, err := config.SystemPromptPath()
	if err != nil {
		t.Fatalf("SystemPromptPath error: %v", err)
	}
	custom := "You are a custom prompt for testing."
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		t.Fatalf("mkdir prompt dir: %v", err)
	}
	if err := os.WriteFile(promptPath, []byte(custom), 0o600); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}

	var firstSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.Messages) > 0 {
			firstSystem = req.Messages[0].Content
		}
		resp := ollama.ChatResponse{
			Message:         ollama.ChatMessage{Role: "assistant", Content: "ok"},
			PromptEvalCount: 4,
			EvalCount:       1,
			Done:            true,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	res, err := engine.HandleText(ctx, "repl", "default", "hello")
	if err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if res.AssistantContent != "ok" {
		t.Fatalf("unexpected assistant content: %q", res.AssistantContent)
	}
	if firstSystem != custom {
		t.Fatalf("expected system prompt %q, got %q", custom, firstSystem)
	}
}

func newTestEngine(t *testing.T, ollamaHost string) (*Engine, *db.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.OllamaHost = ollamaHost
	cfg.DBPath = filepath.Join(t.TempDir(), "state.db")
	cfg.DefaultModel = "test-model"
	cfg.ContextWindowTokens = 10000
	cfg.CompactionThreshold = 0.9
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	client := ollama.NewClient(cfg.OllamaHost)
	pm := plugin.NewManager(store, cfg)
	return New(cfg, store, client, pm, nil), store
}
