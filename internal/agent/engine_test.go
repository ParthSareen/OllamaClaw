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
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/ollama"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
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

func TestHandleTextAttachesInputImagesToLatestUserMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	var seenUserWithImage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		for _, m := range req.Messages {
			if m.Role == "user" && strings.TrimSpace(m.Content) == "what is in this image?" && len(m.Images) == 1 && m.Images[0] == "ZmFrZS1pbWFnZS1iYXNlNjQ=" {
				seenUserWithImage = true
				break
			}
		}
		resp := ollama.ChatResponse{
			Message:         ollama.ChatMessage{Role: "assistant", Content: "looks good"},
			PromptEvalCount: 12,
			EvalCount:       3,
			Done:            true,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	_, err := engine.HandleTextWithOptions(ctx, "telegram", "8750063231", "what is in this image?", HandleOptions{
		InputImages: []string{"ZmFrZS1pbWFnZS1iYXNlNjQ="},
	})
	if err != nil {
		t.Fatalf("HandleTextWithOptions error: %v", err)
	}
	if !seenUserWithImage {
		t.Fatalf("expected user message with image payload in chat request")
	}
}

func TestHandleTextAttachesInputImagesAcrossToolLoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	callCount := 0
	userImageSeenEachCall := []bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		seen := false
		for _, m := range req.Messages {
			if m.Role == "user" && strings.TrimSpace(m.Content) == "check this image and run tools" && len(m.Images) == 1 && m.Images[0] == "aW1hZ2Ux" {
				seen = true
				break
			}
		}
		userImageSeenEachCall = append(userImageSeenEachCall, seen)

		resp := ollama.ChatResponse{}
		if callCount == 1 {
			resp = ollama.ChatResponse{
				Message: ollama.ChatMessage{
					Role: "assistant",
					ToolCalls: []ollama.ToolCall{
						{
							Function: ollama.ToolCallFunction{
								Name:      "missing_tool",
								Arguments: map[string]interface{}{},
							},
						},
					},
				},
				PromptEvalCount: 15,
				EvalCount:       4,
				Done:            true,
			}
		} else {
			resp = ollama.ChatResponse{
				Message:         ollama.ChatMessage{Role: "assistant", Content: "done"},
				PromptEvalCount: 20,
				EvalCount:       5,
				Done:            true,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	_, err := engine.HandleTextWithOptions(ctx, "telegram", "8750063231", "check this image and run tools", HandleOptions{
		InputImages: []string{"aW1hZ2Ux"},
	})
	if err != nil {
		t.Fatalf("HandleTextWithOptions error: %v", err)
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 chat calls, got %d", callCount)
	}
	for i, seen := range userImageSeenEachCall {
		if !seen {
			t.Fatalf("expected input image to be present on request #%d", i+1)
		}
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

func TestSessionDreamingNotificationsRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	engine, store := newTestEngine(t, "http://127.0.0.1:65535")
	defer store.Close()

	enabled, err := engine.IsSessionDreamingNotifications(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("IsSessionDreamingNotifications error: %v", err)
	}
	if !enabled {
		t.Fatalf("expected default dreaming notifications=true")
	}

	if err := engine.SetSessionDreamingNotifications(ctx, "telegram", "123", false); err != nil {
		t.Fatalf("SetSessionDreamingNotifications(false) error: %v", err)
	}
	enabled, err = engine.IsSessionDreamingNotifications(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("IsSessionDreamingNotifications error: %v", err)
	}
	if enabled {
		t.Fatalf("expected dreaming notifications=false after setting off")
	}

	if err := engine.SetSessionDreamingNotifications(ctx, "telegram", "123", true); err != nil {
		t.Fatalf("SetSessionDreamingNotifications(true) error: %v", err)
	}
	enabled, err = engine.IsSessionDreamingNotifications(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("IsSessionDreamingNotifications error: %v", err)
	}
	if !enabled {
		t.Fatalf("expected dreaming notifications=true after setting on")
	}
}

func TestTriggerCoreMemoriesRefreshRunsAndPreventsDuplicate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		resp := ollama.ChatResponse{
			Message:         ollama.ChatMessage{Role: "assistant", Content: "- prefers concise updates"},
			PromptEvalCount: 11,
			EvalCount:       3,
			Done:            true,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	sess, err := engine.GetOrCreateSession(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("GetOrCreateSession error: %v", err)
	}
	if err := store.InsertMessage(ctx, &db.Message{SessionID: sess.ID, Role: "user", Content: "I like short answers"}); err != nil {
		t.Fatalf("InsertMessage error: %v", err)
	}

	events := make(chan CoreMemoryEvent, 4)
	engine.SetCoreMemoryEventSink(func(ev CoreMemoryEvent) {
		events <- ev
	})
	defer engine.SetCoreMemoryEventSink(nil)

	started, err := engine.TriggerCoreMemoriesRefresh(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("TriggerCoreMemoriesRefresh error: %v", err)
	}
	if !started {
		t.Fatalf("expected first trigger to start refresh")
	}

	started, err = engine.TriggerCoreMemoriesRefresh(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("TriggerCoreMemoriesRefresh second call error: %v", err)
	}
	if started {
		t.Fatalf("expected second trigger while in-flight to be skipped")
	}

	close(release)

	var seenStart, seenDone bool
	timeout := time.After(2 * time.Second)
	for !(seenStart && seenDone) {
		select {
		case ev := <-events:
			if ev.Phase == CoreMemoryEventStart {
				seenStart = true
			}
			if ev.Phase == CoreMemoryEventDone {
				seenDone = true
			}
		case <-timeout:
			t.Fatalf("timed out waiting for start+done core memory events")
		}
	}
}

func TestSessionThinkValueRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	engine, store := newTestEngine(t, "http://127.0.0.1:65535")
	defer store.Close()

	value, err := engine.SessionThinkValue(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("SessionThinkValue error: %v", err)
	}
	if value != "off" {
		t.Fatalf("expected default think value off, got %q", value)
	}

	if err := engine.SetSessionThinkValue(ctx, "telegram", "123", "low"); err != nil {
		t.Fatalf("SetSessionThinkValue(low) error: %v", err)
	}
	value, err = engine.SessionThinkValue(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("SessionThinkValue error: %v", err)
	}
	if value != "low" {
		t.Fatalf("expected think value low, got %q", value)
	}
	enabled, err := engine.IsSessionThink(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("IsSessionThink error: %v", err)
	}
	if !enabled {
		t.Fatalf("expected low think value to be treated as enabled")
	}

	if err := engine.SetSessionThinkValue(ctx, "telegram", "123", "default"); err != nil {
		t.Fatalf("SetSessionThinkValue(default) error: %v", err)
	}
	value, err = engine.SessionThinkValue(ctx, "telegram", "123")
	if err != nil {
		t.Fatalf("SessionThinkValue error: %v", err)
	}
	if value != "default" {
		t.Fatalf("expected think value default, got %q", value)
	}

	if err := engine.SetSessionThinkValue(ctx, "telegram", "123", "nope"); err == nil {
		t.Fatalf("expected invalid think value to error")
	}
}

func TestHandleTextSendsExplicitThinkFalse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	var seenThink interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		seenThink = req.Think
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

	if _, err := engine.HandleText(ctx, "repl", "default", "hello"); err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	v, ok := seenThink.(bool)
	if !ok {
		t.Fatalf("expected think field to decode as bool, got %T", seenThink)
	}
	if v {
		t.Fatalf("expected explicit think=false by default")
	}
}

func TestHandleTextSendsThinkLevelString(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	var seenThink interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		seenThink = req.Think
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
	if err := engine.SetSessionThinkValue(ctx, "repl", "default", "high"); err != nil {
		t.Fatalf("SetSessionThinkValue(high) error: %v", err)
	}

	if _, err := engine.HandleText(ctx, "repl", "default", "hello"); err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	v, ok := seenThink.(string)
	if !ok {
		t.Fatalf("expected think field to decode as string, got %T", seenThink)
	}
	if v != "high" {
		t.Fatalf("expected think=high, got %q", v)
	}
}

func TestHandleTextCollectsThinkingTrace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.ChatResponse{
			Message: ollama.ChatMessage{
				Role:     "assistant",
				Content:  "ok",
				Thinking: "first, check the request quickly",
			},
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
	if len(res.ThinkingTrace) != 1 {
		t.Fatalf("expected 1 thinking trace entry, got %d", len(res.ThinkingTrace))
	}
	entry := res.ThinkingTrace[0]
	if entry.Step != 1 {
		t.Fatalf("expected thinking trace step=1, got %d", entry.Step)
	}
	if !strings.Contains(entry.Thinking, "check the request") {
		t.Fatalf("unexpected thinking trace content: %q", entry.Thinking)
	}
	if entry.ToolCallCount != 0 {
		t.Fatalf("expected thinking trace tool count 0, got %d", entry.ToolCallCount)
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
	if !strings.Contains(firstSystem, custom) {
		t.Fatalf("expected system prompt to include custom prompt %q, got %q", custom, firstSystem)
	}
	if !strings.Contains(strings.ToLower(firstSystem), "america/los_angeles") {
		t.Fatalf("expected system prompt to include timezone policy, got %q", firstSystem)
	}
}

func TestHandleTextInjectsCoreMemoriesFromFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()

	corePath, err := config.CoreMemoriesPath()
	if err != nil {
		t.Fatalf("CoreMemoriesPath error: %v", err)
	}
	core := coreMemoriesStartMarker + "\n- prefers terse answers\n- uses telegram heavily\n" + coreMemoriesEndMarker + "\n"
	if err := os.MkdirAll(filepath.Dir(corePath), 0o755); err != nil {
		t.Fatalf("mkdir core memory dir: %v", err)
	}
	if err := os.WriteFile(corePath, []byte(core), 0o600); err != nil {
		t.Fatalf("write core memory file: %v", err)
	}

	var messages []ollama.ChatMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		messages = req.Messages
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

	if _, err := engine.HandleText(ctx, "repl", "default", "hello"); err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if len(messages) < 2 {
		t.Fatalf("expected at least 2 prompt messages, got %d", len(messages))
	}
	if !strings.HasPrefix(messages[1].Content, "Core memories:\n") {
		t.Fatalf("expected core memory system message, got %q", messages[1].Content)
	}
	if strings.Contains(messages[1].Content, coreMemoriesStartMarker) {
		t.Fatalf("expected markers to be stripped from injected memory, got %q", messages[1].Content)
	}
	if !strings.Contains(messages[1].Content, "prefers terse answers") {
		t.Fatalf("expected injected memory content, got %q", messages[1].Content)
	}
}

func TestHandleTextIncludesManagedPromptOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()

	promptPath, err := config.SystemPromptPath()
	if err != nil {
		t.Fatalf("SystemPromptPath error: %v", err)
	}
	overlayPath, err := config.SystemPromptOverlayPath()
	if err != nil {
		t.Fatalf("SystemPromptOverlayPath error: %v", err)
	}
	custom := "You are a custom prompt for testing."
	overlay := "- Prefer short updates.\n- Keep momentum high."
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		t.Fatalf("mkdir prompt dir: %v", err)
	}
	if err := os.WriteFile(promptPath, []byte(custom), 0o600); err != nil {
		t.Fatalf("write prompt file: %v", err)
	}
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay file: %v", err)
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

	if _, err := engine.HandleText(ctx, "repl", "default", "hello"); err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if !strings.Contains(firstSystem, custom) {
		t.Fatalf("expected custom prompt in system message, got %q", firstSystem)
	}
	if !strings.Contains(firstSystem, "Managed Prompt Overlay:") || !strings.Contains(firstSystem, "Prefer short updates") {
		t.Fatalf("expected overlay in system message, got %q", firstSystem)
	}
	if !strings.Contains(strings.ToLower(firstSystem), "america/los_angeles") {
		t.Fatalf("expected timezone policy in system message, got %q", firstSystem)
	}
}

func TestHandleTextInjectsPrefetchedBashAsToolContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	ctx = tools.WithPrefetchedBashResults(ctx, []tools.PrefetchedBashResult{
		{
			Command:    "pwd",
			RunID:      "run-abc123",
			RunStarted: "2026-04-08T17:04:59-07:00",
			FetchedAt:  "2026-04-08T17:05:00-07:00",
			ExitCode:   0,
			Stdout:     "/tmp",
			Stderr:     "",
			DurationMs: 5,
		},
	})

	var firstReq []ollama.ChatMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		firstReq = req.Messages
		resp := ollama.ChatResponse{
			Message:         ollama.ChatMessage{Role: "assistant", Content: "ok"},
			PromptEvalCount: 10,
			EvalCount:       2,
			Done:            true,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()
	if _, err := engine.HandleText(ctx, "telegram", "8750063231", "check status"); err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if len(firstReq) < 4 {
		t.Fatalf("expected at least 4 messages (system + assistant/tool prefetch + user), got %d", len(firstReq))
	}
	assistantIdx := -1
	toolIdx := -1
	userIdx := -1
	for i, m := range firstReq {
		if assistantIdx == -1 && m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].Function.Name == "bash" {
			assistantIdx = i
		}
		if toolIdx == -1 && m.Role == "tool" && m.ToolName == "bash" && strings.Contains(m.Content, "\"prefetched\":true") {
			toolIdx = i
			if !strings.Contains(m.Content, "\"run_started_at\":\"2026-04-08T17:04:59-07:00\"") {
				t.Fatalf("expected run_started_at in prefetched tool result, got %q", m.Content)
			}
			if !strings.Contains(m.Content, "\"run_id\":\"run-abc123\"") {
				t.Fatalf("expected run_id in prefetched tool result, got %q", m.Content)
			}
		}
		if userIdx == -1 && m.Role == "user" && strings.TrimSpace(m.Content) == "check status" {
			userIdx = i
		}
	}
	if assistantIdx == -1 {
		t.Fatalf("expected synthetic assistant bash tool call in prompt, got %#v", firstReq)
	}
	if toolIdx == -1 {
		t.Fatalf("expected synthetic prefetched tool result in prompt, got %#v", firstReq)
	}
	if userIdx == -1 {
		t.Fatalf("expected user prompt message in request, got %#v", firstReq)
	}
	if !(assistantIdx < toolIdx && toolIdx < userIdx) {
		t.Fatalf("expected order assistant(tool call) -> tool(result) -> user(prompt); got assistant=%d tool=%d user=%d messages=%#v", assistantIdx, toolIdx, userIdx, firstReq)
	}
	sess, err := engine.GetOrCreateSession(context.Background(), "telegram", "8750063231")
	if err != nil {
		t.Fatalf("GetOrCreateSession error: %v", err)
	}
	active, err := store.ListMessages(context.Background(), sess.ID, false)
	if err != nil {
		t.Fatalf("ListMessages error: %v", err)
	}
	for _, m := range active {
		if strings.HasPrefix(m.ToolCallID, prefetchToolIDPrefix) {
			t.Fatalf("expected synthetic prefetch context to be archived after run, found active message: %+v", m)
		}
	}
}

func TestHandleTextPrefetchedContextRequiresRunID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	ctx = tools.WithPrefetchedBashResults(ctx, []tools.PrefetchedBashResult{
		{
			Command:    "pwd",
			RunStarted: "2026-04-08T17:04:59-07:00",
			FetchedAt:  "2026-04-08T17:05:00-07:00",
			ExitCode:   0,
		},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("ollama should not be called when prefetch run_id is invalid")
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()
	if _, err := engine.HandleText(ctx, "telegram", "8750063231", "check status"); err == nil {
		t.Fatalf("expected error when prefetch run_id is missing")
	} else if !strings.Contains(err.Error(), "run_id") {
		t.Fatalf("expected run_id error, got %v", err)
	}
}

func TestUpsertManagedCoreMemoriesPreservesUserContent(t *testing.T) {
	existing := strings.Join([]string{
		"# Notes",
		"Keep this line",
		"",
		coreMemoriesStartMarker,
		"- old memory",
		coreMemoriesEndMarker,
		"",
		"Manual footer",
	}, "\n")
	updated := upsertManagedCoreMemories(existing, "- new memory")
	if !strings.Contains(updated, "Keep this line") || !strings.Contains(updated, "Manual footer") {
		t.Fatalf("expected non-managed content to be preserved, got %q", updated)
	}
	if strings.Contains(updated, "- old memory") {
		t.Fatalf("expected managed section to be replaced, got %q", updated)
	}
	if !strings.Contains(updated, "- new memory") {
		t.Fatalf("expected new managed content, got %q", updated)
	}
}

func TestClampToMaxChars(t *testing.T) {
	in := strings.Repeat("a", coreMemoriesMaxChars+37)
	got := clampToMaxChars(in, coreMemoriesMaxChars)
	if len([]rune(got)) != coreMemoriesMaxChars {
		t.Fatalf("expected %d chars, got %d", coreMemoriesMaxChars, len([]rune(got)))
	}
}

func TestHandleTextInjectsClampedCoreMemories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()

	corePath, err := config.CoreMemoriesPath()
	if err != nil {
		t.Fatalf("CoreMemoriesPath error: %v", err)
	}
	longMem := "- " + strings.Repeat("x", coreMemoriesMaxChars+250)
	core := coreMemoriesStartMarker + "\n" + longMem + "\n" + coreMemoriesEndMarker + "\n"
	if err := os.MkdirAll(filepath.Dir(corePath), 0o755); err != nil {
		t.Fatalf("mkdir core memory dir: %v", err)
	}
	if err := os.WriteFile(corePath, []byte(core), 0o600); err != nil {
		t.Fatalf("write core memory file: %v", err)
	}

	var messages []ollama.ChatMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		messages = req.Messages
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

	if _, err := engine.HandleText(ctx, "repl", "default", "hello"); err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if len(messages) < 2 {
		t.Fatalf("expected at least 2 prompt messages, got %d", len(messages))
	}
	injected := strings.TrimPrefix(messages[1].Content, "Core memories:\n")
	if len([]rune(injected)) > coreMemoriesMaxChars {
		t.Fatalf("expected injected core memories <= %d chars, got %d", coreMemoriesMaxChars, len([]rune(injected)))
	}
}

func TestWithTimezonePolicyPromptAppendsOnce(t *testing.T) {
	base := "You are custom."
	once := withTimezonePolicyPrompt(base)
	if !strings.Contains(strings.ToLower(once), "america/los_angeles") {
		t.Fatalf("expected timezone policy to be appended, got %q", once)
	}
	twice := withTimezonePolicyPrompt(once)
	if twice != once {
		t.Fatalf("expected second application to be idempotent, got %q", twice)
	}
}

func TestWithCurrentTimePromptAddsPacificAndUTC(t *testing.T) {
	base := "You are custom."
	now := time.Date(2026, time.April, 15, 18, 45, 30, 0, time.UTC)
	got := withCurrentTimePrompt(base, now)
	if !strings.Contains(got, "Current runtime time:") {
		t.Fatalf("expected current runtime time section, got %q", got)
	}
	if !strings.Contains(got, "Current time (America/Los_Angeles): 2026-04-15T11:45:30-07:00") {
		t.Fatalf("expected pacific timestamp in prompt, got %q", got)
	}
	if !strings.Contains(got, "Current time (UTC): 2026-04-15T18:45:30Z") {
		t.Fatalf("expected UTC timestamp in prompt, got %q", got)
	}
}

func TestWithCurrentTimePromptAppendsOnce(t *testing.T) {
	base := "You are custom."
	now := time.Date(2026, time.April, 15, 18, 45, 30, 0, time.UTC)
	once := withCurrentTimePrompt(base, now)
	twice := withCurrentTimePrompt(once, now.Add(2*time.Hour))
	if twice != once {
		t.Fatalf("expected second current-time application to be idempotent, got %q", twice)
	}
}

func TestFullSystemContextIncludesCoreAndCompactionSummary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("FullSystemContext should not call /api/chat")
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	sess, err := engine.GetOrCreateSession(ctx, "telegram", "8750063231")
	if err != nil {
		t.Fatalf("GetOrCreateSession error: %v", err)
	}
	if err := store.InsertCompaction(ctx, db.Compaction{
		SessionID:         sess.ID,
		Summary:           "User prefers concise updates and PST timestamps.",
		FirstKeptMessage:  0,
		ArchivedBeforeSeq: 0,
	}); err != nil {
		t.Fatalf("InsertCompaction error: %v", err)
	}

	corePath, err := config.CoreMemoriesPath()
	if err != nil {
		t.Fatalf("CoreMemoriesPath error: %v", err)
	}
	core := coreMemoriesStartMarker + "\n- User likes terse but warm responses.\n" + coreMemoriesEndMarker + "\n"
	if err := os.MkdirAll(filepath.Dir(corePath), 0o755); err != nil {
		t.Fatalf("mkdir core memory dir: %v", err)
	}
	if err := os.WriteFile(corePath, []byte(core), 0o600); err != nil {
		t.Fatalf("write core memories file: %v", err)
	}

	full, err := engine.FullSystemContext(ctx, "telegram", "8750063231")
	if err != nil {
		t.Fatalf("FullSystemContext error: %v", err)
	}
	if !strings.Contains(full, "System prompt:\n") {
		t.Fatalf("expected system prompt section, got %q", full)
	}
	if !strings.Contains(full, "Current runtime time:") {
		t.Fatalf("expected runtime time in system prompt, got %q", full)
	}
	if !strings.Contains(full, "Core memories:\n- User likes terse but warm responses.") {
		t.Fatalf("expected core memories section, got %q", full)
	}
	if !strings.Contains(full, "Conversation summary:\nUser prefers concise updates and PST timestamps.") {
		t.Fatalf("expected conversation summary section, got %q", full)
	}
}

func TestEstimateNextPromptUsesJSONHeuristic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("EstimateNextPrompt should not call /api/chat")
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()

	sess, err := engine.GetOrCreateSession(ctx, "telegram", "8750063231")
	if err != nil {
		t.Fatalf("GetOrCreateSession error: %v", err)
	}
	if err := store.InsertMessage(ctx, &db.Message{SessionID: sess.ID, Role: "user", Content: "hello"}); err != nil {
		t.Fatalf("InsertMessage error: %v", err)
	}

	estimate, err := engine.EstimateNextPrompt(ctx, "telegram", "8750063231")
	if err != nil {
		t.Fatalf("EstimateNextPrompt error: %v", err)
	}
	if estimate.EstimatorFormula != "len(request_json)/4" {
		t.Fatalf("unexpected estimator formula: %q", estimate.EstimatorFormula)
	}
	if estimate.RequestChars <= 0 {
		t.Fatalf("expected positive request chars, got %d", estimate.RequestChars)
	}
	if estimate.EstimatedTokens <= 0 {
		t.Fatalf("expected positive estimated tokens, got %d", estimate.EstimatedTokens)
	}
	if estimate.EstimatedTokens != estimate.RequestChars/4 {
		t.Fatalf("expected tokens to match chars/4, got chars=%d tokens=%d", estimate.RequestChars, estimate.EstimatedTokens)
	}
	if estimate.MessageCount < 2 {
		t.Fatalf("expected at least system+user message count, got %d", estimate.MessageCount)
	}
	if estimate.ToolCount == 0 {
		t.Fatalf("expected built-in tools to be counted")
	}
}

func TestHandleTextRejectsOversizedPromptBeforeChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("oversized prompt should not call /api/chat")
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()
	engine.cfg.ContextWindowTokens = 20

	_, err := engine.HandleText(ctx, "telegram", "8750063231", strings.Repeat("large prompt ", 100))
	if err == nil {
		t.Fatalf("expected oversized prompt error")
	}
	if !strings.Contains(err.Error(), "prompt is too large") {
		t.Fatalf("expected prompt size error, got %v", err)
	}
}

func TestCompactionSummaryOmitsThinking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		callCount++
		resp := ollama.ChatResponse{
			Message:         ollama.ChatMessage{Role: "assistant", Content: "final"},
			PromptEvalCount: 20,
			EvalCount:       1,
			Done:            true,
		}
		if callCount == 2 {
			if len(req.Messages) < 2 {
				t.Fatalf("expected summary request to include user payload")
			}
			if strings.Contains(req.Messages[1].Content, "secret chain of thought") {
				t.Fatalf("compaction summary payload leaked thinking: %s", req.Messages[1].Content)
			}
			resp.Message.Content = "summary without thinking"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	engine, store := newTestEngine(t, srv.URL)
	defer store.Close()
	engine.cfg.CompactionThreshold = 0.001
	engine.cfg.KeepRecentTurns = 1

	sess, err := engine.GetOrCreateSession(ctx, "telegram", "8750063231")
	if err != nil {
		t.Fatalf("GetOrCreateSession error: %v", err)
	}
	if err := store.InsertMessage(ctx, &db.Message{SessionID: sess.ID, Role: "user", Content: "old request"}); err != nil {
		t.Fatalf("InsertMessage user error: %v", err)
	}
	if err := store.InsertMessage(ctx, &db.Message{SessionID: sess.ID, Role: "assistant", Content: "old answer", Thinking: "secret chain of thought"}); err != nil {
		t.Fatalf("InsertMessage assistant error: %v", err)
	}

	res, err := engine.HandleText(ctx, "telegram", "8750063231", "new request")
	if err != nil {
		t.Fatalf("HandleText error: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("expected compaction to run")
	}
	if callCount != 2 {
		t.Fatalf("expected chat + summary calls, got %d", callCount)
	}
}

func TestSummarizeCoreMemoryDelta(t *testing.T) {
	before := "- prefers concise replies\n- runs cron checks in morning\n- tone: founder copilot"
	after := "- prefers concise replies\n- tone: founder copilot\n- asks for PST timestamps\n- likes command previews"
	delta := summarizeCoreMemoryDelta(before, after)

	if delta.AddedCount != 2 {
		t.Fatalf("expected AddedCount=2, got %d", delta.AddedCount)
	}
	if delta.RemovedCount != 1 {
		t.Fatalf("expected RemovedCount=1, got %d", delta.RemovedCount)
	}
	if delta.KeptCount != 2 {
		t.Fatalf("expected KeptCount=2, got %d", delta.KeptCount)
	}
	if len(delta.AddedPreview) != 2 {
		t.Fatalf("expected two added previews, got %v", delta.AddedPreview)
	}
	if len(delta.RemovedPreview) != 1 {
		t.Fatalf("expected one removed preview, got %v", delta.RemovedPreview)
	}
	if delta.BeforeChars <= 0 || delta.AfterChars <= 0 {
		t.Fatalf("expected non-zero char counts, got before=%d after=%d", delta.BeforeChars, delta.AfterChars)
	}
}

func TestSummarizeCoreMemoryDeltaNormalizesListPrefixes(t *testing.T) {
	before := "1. Keeps responses concise\n2. Uses PST"
	after := "- Keeps responses concise\n- Uses PST"
	delta := summarizeCoreMemoryDelta(before, after)
	if delta.AddedCount != 0 || delta.RemovedCount != 0 {
		t.Fatalf("expected normalized items to match exactly, got %+v", delta)
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
	return New(cfg, store, client, nil), store
}
