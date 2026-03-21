package db

import (
	"context"
	"path/filepath"
	"testing"
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
}
