package subagents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
)

func TestManagerRunsGenericCodexTask(t *testing.T) {
	store, cleanup := openTestStore(t)
	defer cleanup()
	root := filepath.Join(t.TempDir(), "subagents")
	workdir := t.TempDir()
	fakeCodex := writeFakeCodex(t)
	mgr := NewManager(config.SubagentConfig{
		Enabled:                true,
		CodexBinary:            fakeCodex,
		RootDir:                root,
		MaxConcurrent:          1,
		DefaultTimeoutMinutes:  1,
		DefaultReasoningEffort: "xhigh",
		Sandbox:                "workspace-write",
	}, store)

	delivered := make(chan string, 1)
	mgr.SetOutputSink(func(ctx context.Context, transport, sessionKey, content string) error {
		delivered <- content
		return nil
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	defer mgr.Stop()

	info, err := mgr.AddSubagentTask(context.Background(), tools.SubagentSpec{
		Prompt:     "hello from test",
		Transport:  "telegram",
		SessionKey: "123",
		Workdir:    workdir,
	})
	if err != nil {
		t.Fatalf("add task: %v", err)
	}

	final := waitForTaskStatus(t, store, info.ID, statusSucceeded)
	if final.ResultPath == "" {
		t.Fatalf("expected result path")
	}
	result, ok, err := mgr.GetSubagentResult(context.Background(), info.ID)
	if err != nil || !ok {
		t.Fatalf("get result ok=%t err=%v", ok, err)
	}
	if !strings.Contains(result.Content, "fake result: hello from test") {
		t.Fatalf("unexpected result content: %q", result.Content)
	}
	select {
	case msg := <-delivered:
		if !strings.Contains(msg, info.ID) || !strings.Contains(msg, "fake result: hello from test") {
			t.Fatalf("unexpected delivery: %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("expected delivery")
	}
}

func TestManagerCancelsQueuedTask(t *testing.T) {
	store, cleanup := openTestStore(t)
	defer cleanup()
	mgr := NewManager(config.SubagentConfig{
		Enabled:               true,
		CodexBinary:           writeFakeCodex(t),
		RootDir:               filepath.Join(t.TempDir(), "subagents"),
		MaxConcurrent:         1,
		DefaultTimeoutMinutes: 1,
		Sandbox:               "workspace-write",
	}, store)

	info, err := mgr.AddSubagentTask(context.Background(), tools.SubagentSpec{
		Prompt:  "do not run",
		Workdir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("add task: %v", err)
	}
	canceled, err := mgr.CancelSubagentTask(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("cancel task: %v", err)
	}
	if canceled.Status != statusCanceled {
		t.Fatalf("expected canceled, got %s", canceled.Status)
	}
}

func TestNormalizePRRef(t *testing.T) {
	for _, tc := range []struct {
		name        string
		raw         string
		defaultRepo string
		wantRepo    string
		wantNumber  int
	}{
		{name: "bare", raw: "12", defaultRepo: "owner/repo", wantRepo: "owner/repo", wantNumber: 12},
		{name: "shorthand", raw: "owner/repo#34", wantRepo: "owner/repo", wantNumber: 34},
		{name: "url", raw: "https://github.com/owner/repo/pull/56", wantRepo: "owner/repo", wantNumber: 56},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizePRRef(tc.raw, tc.defaultRepo)
			if err != nil {
				t.Fatalf("normalize PR ref: %v", err)
			}
			if got.Repo != tc.wantRepo || got.Number != tc.wantNumber {
				t.Fatalf("got repo=%q number=%d", got.Repo, got.Number)
			}
		})
	}
}

func TestCodexArgsUseProfileLongFlag(t *testing.T) {
	mgr := NewManager(config.SubagentConfig{
		CodexBinary:            "codex",
		RootDir:                t.TempDir(),
		MaxConcurrent:          1,
		DefaultTimeoutMinutes:  1,
		DefaultProfile:         "reviewer",
		DefaultReasoningEffort: "xhigh",
		Sandbox:                "workspace-write",
	}, nil)
	args := mgr.codexArgs(db.SubagentTask{
		ID:         "agent-test",
		Kind:       kindGeneric,
		ResultPath: "/tmp/result.md",
	}, "/tmp/repo")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, " -p ") {
		t.Fatalf("codex args must not use -p for prompts/profile: %v", args)
	}
	if !containsArgPair(args, "--profile", "reviewer") {
		t.Fatalf("expected --profile reviewer in args: %v", args)
	}
	if !containsArgPair(args, "-c", `model_reasoning_effort="xhigh"`) {
		t.Fatalf("expected xhigh reasoning config in args: %v", args)
	}
	if args[len(args)-1] != "-" {
		t.Fatalf("expected stdin prompt marker, got args: %v", args)
	}
}

func openTestStore(t *testing.T) (*db.Store, func()) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store, func() { _ = store.Close() }
}

func containsArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func writeFakeCodex(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    out="$1"
  fi
  shift
done
input="$(cat)"
printf 'fake result: %s\n' "$input" > "$out"
printf '{"event":"done"}\n'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}

func waitForTaskStatus(t *testing.T, store *db.Store, id, status string) db.SubagentTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, ok, err := store.GetSubagentTask(context.Background(), id)
		if err != nil {
			t.Fatalf("get task: %v", err)
		}
		if !ok {
			t.Fatalf("task not found: %s", id)
		}
		if task.Status == status {
			return task
		}
		if task.Status == statusFailed || task.Status == statusCanceled {
			t.Fatalf("task ended with status=%s error=%s", task.Status, task.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", status)
	return db.SubagentTask{}
}
