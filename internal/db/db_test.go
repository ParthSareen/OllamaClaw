package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrationsAndSessionLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetOrCreateActiveSession(ctx, "repl", "default", "model-a")
	if err != nil {
		t.Fatalf("GetOrCreateActiveSession() error: %v", err)
	}
	if sess.ModelOverride != "model-a" {
		t.Fatalf("expected model-a, got %s", sess.ModelOverride)
	}

	reset, err := store.ResetSession(ctx, "repl", "default", "model-b")
	if err != nil {
		t.Fatalf("ResetSession() error: %v", err)
	}
	if reset.ModelOverride != "model-b" {
		t.Fatalf("expected model-b, got %s", reset.ModelOverride)
	}
}

func TestMessageArchiveAndCompactionSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	sess, err := store.CreateSession(ctx, "repl", "default", "model")
	if err != nil {
		t.Fatal(err)
	}
	m1 := &Message{SessionID: sess.ID, Role: "user", Content: "u1"}
	m2 := &Message{SessionID: sess.ID, Role: "assistant", Content: "a1"}
	if err := store.InsertMessage(ctx, m1); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessage(ctx, m2); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertCompaction(ctx, Compaction{SessionID: sess.ID, Summary: "summary", FirstKeptMessage: m2.ID, ArchivedBeforeSeq: m2.Seq}); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveMessagesByIDs(ctx, sess.ID, []int64{m1.ID}); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListMessages(ctx, sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != m2.ID {
		t.Fatalf("expected one active message m2, got %+v", active)
	}
	summary, ok, err := store.LatestCompactionSummary(ctx, sess.ID)
	if err != nil || !ok {
		t.Fatalf("expected summary, got ok=%v err=%v", ok, err)
	}
	if summary != "summary" {
		t.Fatalf("unexpected summary %q", summary)
	}
	countUser, err := store.CountMessagesByRole(ctx, sess.ID, "user")
	if err != nil {
		t.Fatalf("CountMessagesByRole(user) error: %v", err)
	}
	if countUser != 1 {
		t.Fatalf("expected 1 user message, got %d", countUser)
	}
	countTool, err := store.CountMessagesByRole(ctx, sess.ID, "tool")
	if err != nil {
		t.Fatalf("CountMessagesByRole(tool) error: %v", err)
	}
	if countTool != 0 {
		t.Fatalf("expected 0 tool messages, got %d", countTool)
	}
}

func TestArchiveMessagesByToolCallID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	sess, err := store.CreateSession(ctx, "telegram", "123", "model")
	if err != nil {
		t.Fatal(err)
	}
	m1 := &Message{SessionID: sess.ID, Role: "assistant", Content: "prefetch 1", ToolCallID: "prefetch_ctx"}
	m2 := &Message{SessionID: sess.ID, Role: "tool", Content: `{"prefetched":true}`, ToolName: "bash", ToolCallID: "prefetch_ctx"}
	m3 := &Message{SessionID: sess.ID, Role: "assistant", Content: "normal"}
	if err := store.InsertMessage(ctx, m1); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessage(ctx, m2); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessage(ctx, m3); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveMessagesByToolCallID(ctx, sess.ID, "prefetch_ctx"); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListMessages(ctx, sess.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != m3.ID {
		t.Fatalf("expected only non-prefetch message to remain active, got %+v", active)
	}
}

func TestCronJobSafePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	job := CronJob{
		ID:           "job-safe",
		Schedule:     "0 * * * *",
		Prompt:       "ping",
		Transport:    "telegram",
		SessionKey:   "123",
		Active:       true,
		Safe:         false,
		AutoPrefetch: true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := store.UpsertCronJob(ctx, job); err != nil {
		t.Fatalf("UpsertCronJob() error: %v", err)
	}
	got, ok, err := store.GetCronJob(ctx, job.ID)
	if err != nil || !ok {
		t.Fatalf("GetCronJob() failed ok=%t err=%v", ok, err)
	}
	if got.Safe {
		t.Fatalf("expected safe=false after initial upsert")
	}

	job.Safe = true
	if err := store.UpsertCronJob(ctx, job); err != nil {
		t.Fatalf("UpsertCronJob() safe=true error: %v", err)
	}
	got, ok, err = store.GetCronJob(ctx, job.ID)
	if err != nil || !ok {
		t.Fatalf("GetCronJob() failed after safe update ok=%t err=%v", ok, err)
	}
	if !got.Safe {
		t.Fatalf("expected safe=true after update")
	}

	jobs, err := store.ListCronJobs(ctx, true)
	if err != nil {
		t.Fatalf("ListCronJobs() error: %v", err)
	}
	if len(jobs) != 1 || !jobs[0].Safe {
		t.Fatalf("expected one safe job in listing, got %+v", jobs)
	}
}

func TestMigrateV2CronJobsAddsSafeColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error: %v", err)
	}
	defer raw.Close()

	ctx := context.Background()
	_, err = raw.ExecContext(ctx, `
CREATE TABLE schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);
INSERT INTO schema_migrations(version, applied_at) VALUES(1, '2026-01-01T00:00:00Z');
INSERT INTO schema_migrations(version, applied_at) VALUES(2, '2026-01-01T00:00:01Z');
CREATE TABLE cron_jobs (
  id TEXT PRIMARY KEY,
  schedule TEXT NOT NULL,
  prompt TEXT NOT NULL,
  transport TEXT NOT NULL,
  session_key TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  last_run_at TEXT NULL,
  next_run_at TEXT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO cron_jobs(id, schedule, prompt, transport, session_key, active, created_at, updated_at)
VALUES('legacy-job', '0 * * * *', 'ping', 'telegram', '123', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
`)
	if err != nil {
		t.Fatalf("seed v2 schema error: %v", err)
	}
	_ = raw.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() migration error: %v", err)
	}
	defer store.Close()

	job, ok, err := store.GetCronJob(context.Background(), "legacy-job")
	if err != nil || !ok {
		t.Fatalf("GetCronJob() failed after migration ok=%t err=%v", ok, err)
	}
	if job.Safe {
		t.Fatalf("expected migrated job safe=false by default")
	}
	if !job.AutoPrefetch {
		t.Fatalf("expected migrated job auto_prefetch=true by default")
	}
}

func TestCronPrefetchCommandsPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.UpsertCronJob(ctx, CronJob{
		ID:           "job-prefetch",
		Schedule:     "0 * * * *",
		Prompt:       "ping",
		Transport:    "telegram",
		SessionKey:   "123",
		Active:       true,
		AutoPrefetch: true,
	}); err != nil {
		t.Fatalf("UpsertCronJob() error: %v", err)
	}

	if err := store.UpsertCronPrefetchCommands(ctx, "job-prefetch", []string{
		"gh pr view 1 --json number,title",
		"gh pr view 1 --json number,title",
		"pwd",
	}); err != nil {
		t.Fatalf("UpsertCronPrefetchCommands() error: %v", err)
	}

	commands, err := store.ListCronPrefetchCommands(ctx, "job-prefetch")
	if err != nil {
		t.Fatalf("ListCronPrefetchCommands() error: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("expected deduped 2 commands, got %d (%v)", len(commands), commands)
	}
}
