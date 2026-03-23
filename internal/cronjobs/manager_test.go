package cronjobs

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/parth/ollamaclaw/internal/db"
	"github.com/parth/ollamaclaw/internal/tools"
)

func TestAddListRemoveJob(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	mgr := NewManager(store)
	mgr.SetRunner(func(ctx context.Context, transport, sessionKey, prompt string) (string, error) {
		return "ok", nil
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer mgr.Stop()

	job, err := mgr.AddJob(context.Background(), tools.CronJobSpec{
		ID:         "job-test",
		Schedule:   "0 * * * *",
		Prompt:     "ping",
		Transport:  "repl",
		SessionKey: "default",
	})
	if err != nil {
		t.Fatalf("add job: %v", err)
	}
	if job.ID != "job-test" {
		t.Fatalf("unexpected id %s", job.ID)
	}
	jobs, err := mgr.ListJobs(context.Background(), true)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if err := mgr.RemoveJob(context.Background(), "job-test"); err != nil {
		t.Fatalf("remove job: %v", err)
	}
	jobs, err = mgr.ListJobs(context.Background(), false)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}
