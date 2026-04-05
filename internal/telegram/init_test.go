package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type tokenWrappedErr struct {
	msg string
	err error
}

func (e tokenWrappedErr) Error() string {
	return e.msg
}

func (e tokenWrappedErr) Unwrap() error {
	return e.err
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRedactTelegramError(t *testing.T) {
	token := "123456:abcDEF"
	sentinel := errors.New("sentinel")
	base := tokenWrappedErr{
		msg: fmt.Sprintf("telegram request failed: https://api.telegram.org/bot%s/getMe", token),
		err: sentinel,
	}

	redacted := redactTelegramError(token, base)
	if redacted == nil {
		t.Fatalf("expected redacted error")
	}
	if strings.Contains(redacted.Error(), token) {
		t.Fatalf("expected token to be redacted, got %q", redacted.Error())
	}
	if !errors.Is(redacted, sentinel) {
		t.Fatalf("expected redacted error to preserve unwrap chain")
	}
}

func TestCallHonorsCallerContextDeadline(t *testing.T) {
	oldTimeout := telegramAPICallTimeout
	oldTransport := http.DefaultClient.Transport
	t.Cleanup(func() {
		telegramAPICallTimeout = oldTimeout
		http.DefaultClient.Transport = oldTransport
	})

	telegramAPICallTimeout = 2 * time.Second
	http.DefaultClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(500 * time.Millisecond):
			return nil, fmt.Errorf("request context was not canceled")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := call(ctx, "123456:abcDEF", "getMe", nil)
	if err == nil {
		t.Fatalf("expected deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context deadline/cancel error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("call took too long: %s", elapsed)
	}
}

func TestCallHonorsBoundedTimeout(t *testing.T) {
	oldTimeout := telegramAPICallTimeout
	oldTransport := http.DefaultClient.Transport
	t.Cleanup(func() {
		telegramAPICallTimeout = oldTimeout
		http.DefaultClient.Transport = oldTransport
	})

	telegramAPICallTimeout = 25 * time.Millisecond
	http.DefaultClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(500 * time.Millisecond):
			return nil, fmt.Errorf("request context was not canceled")
		}
	})

	start := time.Now()
	_, err := call(context.Background(), "123456:abcDEF", "getMe", nil)
	if err == nil {
		t.Fatalf("expected bounded timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded timeout took too long: %s", elapsed)
	}
}

func TestBotCommandDefinitionsIncludesStopShowAndRestart(t *testing.T) {
	cmds := botCommandDefinitions()
	got := map[string]bool{}
	for _, c := range cmds {
		got[c["command"]] = true
	}
	for _, want := range []string{"start", "help", "show", "status", "stop", "restart"} {
		if !got[want] {
			t.Fatalf("expected command %q to be present", want)
		}
	}
}

func TestSyncCommandsSetsDefaultAndPrivateScopes(t *testing.T) {
	oldTransport := http.DefaultClient.Transport
	t.Cleanup(func() {
		http.DefaultClient.Transport = oldTransport
	})

	type observed struct {
		method string
		body   map[string]interface{}
	}
	var (
		mu   sync.Mutex
		seen []observed
	)

	http.DefaultClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)

		path := req.URL.Path
		method := path[strings.LastIndex(path, "/")+1:]
		mu.Lock()
		seen = append(seen, observed{method: method, body: body})
		mu.Unlock()

		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
			Header:     make(http.Header),
		}, nil
	})

	if err := SyncCommands(context.Background(), "123456:abcDEF"); err != nil {
		t.Fatalf("SyncCommands error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 telegram calls, got %d", len(seen))
	}
	if seen[0].method != "setMyCommands" || seen[1].method != "setMyCommands" {
		t.Fatalf("expected setMyCommands calls, got %+v", seen)
	}
	if _, ok := seen[0].body["scope"]; ok {
		t.Fatalf("default command sync should not include scope")
	}
	scopeRaw, ok := seen[1].body["scope"]
	if !ok {
		t.Fatalf("private command sync missing scope")
	}
	scope, ok := scopeRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("scope payload has unexpected type: %T", scopeRaw)
	}
	if scopeType, _ := scope["type"].(string); scopeType != "all_private_chats" {
		t.Fatalf("unexpected scope type: %q", scopeType)
	}
}
