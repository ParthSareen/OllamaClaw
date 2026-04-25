package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatRetriesTransientStatusAndSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		call := calls.Add(1)
		if call == 1 {
			http.Error(w, "temporary overload", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Message: ChatMessage{Role: "assistant", Content: "ok"},
			Done:    true,
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	client.chatRetryBaseDelay = 0
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:    "test",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Message.Content != "ok" {
		t.Fatalf("unexpected response content %q", resp.Message.Content)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 calls, got %d", got)
	}
}

func TestChatRetriesRateLimitAndStopsAfterMaxAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	client.chatRetryBaseDelay = 0
	_, err := client.Chat(context.Background(), ChatRequest{
		Model:    "test",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected Chat() error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("expected final 429 error, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected initial call plus 2 retries, got %d", got)
	}
}

func TestChatDoesNotRetryNonRetryableStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	client.chatRetryBaseDelay = 0
	_, err := client.Chat(context.Background(), ChatRequest{
		Model:    "test",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected Chat() error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected no retry for 400, got %d calls", got)
	}
}

func TestChatDoesNotRetryCanceledContext(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	client.chatRetryBaseDelay = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Chat(ctx, ChatRequest{
		Model:    "test",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected no server calls after context cancellation, got %d", got)
	}
}

func TestChatRetryDelayAddsJitterAndCaps(t *testing.T) {
	client := NewClient("http://localhost:11434")
	client.chatRetryBaseDelay = 10 * time.Millisecond
	client.chatRetryMaxDelay = 15 * time.Millisecond

	for i := 0; i < 100; i++ {
		delay := client.chatRetryDelay(3)
		if delay < 15*time.Millisecond {
			t.Fatalf("expected capped base delay at least 15ms, got %s", delay)
		}
		if delay > 22*time.Millisecond+500*time.Microsecond {
			t.Fatalf("expected jittered delay near cap+50%%, got %s", delay)
		}
	}
}
