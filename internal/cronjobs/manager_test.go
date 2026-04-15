package cronjobs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
	"github.com/ParthSareen/OllamaClaw/internal/util"
	"github.com/robfig/cron/v3"
)

func TestAddListRemoveJob(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	mgr := NewManager(store)
	mgr.SetRunner(func(ctx context.Context, transport, sessionKey, prompt string) (RunResult, error) {
		return RunResult{Output: "ok"}, nil
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
	if job.Safe {
		t.Fatalf("expected new job safe=false by default")
	}
	if !job.AutoPrefetch {
		t.Fatalf("expected new job auto_prefetch=true by default")
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

func TestSetJobSafeAndApproverInjection(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	mgr := NewManager(store)
	var (
		lastHadApprover bool
		runCount        int
	)
	mgr.SetRunner(func(ctx context.Context, transport, sessionKey, prompt string) (RunResult, error) {
		_, lastHadApprover = tools.BashApproverFromContext(ctx)
		runCount++
		return RunResult{Output: "ok"}, nil
	})
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer mgr.Stop()

	job, err := mgr.AddJob(context.Background(), tools.CronJobSpec{
		ID:         "job-safe",
		Schedule:   "0 * * * *",
		Prompt:     "ping",
		Transport:  "telegram",
		SessionKey: "8750063231",
	})
	if err != nil {
		t.Fatalf("add job: %v", err)
	}

	lastHadApprover = false
	mgr.runJob(job.ID)
	if runCount != 1 {
		t.Fatalf("expected runCount=1 after first run, got %d", runCount)
	}
	if lastHadApprover {
		t.Fatalf("expected approver absent for safe=false jobs")
	}

	updated, err := mgr.SetJobSafe(context.Background(), job.ID, true)
	if err != nil {
		t.Fatalf("SetJobSafe(true) error: %v", err)
	}
	if !updated.Safe {
		t.Fatalf("expected SetJobSafe(true) to return safe=true")
	}

	lastHadApprover = false
	mgr.runJob(job.ID)
	if runCount != 2 {
		t.Fatalf("expected runCount=2 after second run, got %d", runCount)
	}
	if !lastHadApprover {
		t.Fatalf("expected approver present for safe=true jobs")
	}

	updated, err = mgr.SetJobSafe(context.Background(), job.ID, false)
	if err != nil {
		t.Fatalf("SetJobSafe(false) error: %v", err)
	}
	if updated.Safe {
		t.Fatalf("expected SetJobSafe(false) to return safe=false")
	}

	lastHadApprover = false
	mgr.runJob(job.ID)
	if runCount != 3 {
		t.Fatalf("expected runCount=3 after third run, got %d", runCount)
	}
	if lastHadApprover {
		t.Fatalf("expected approver absent after safe=false")
	}
}

func TestSetJobSafeMissingJob(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	mgr := NewManager(store)
	if _, err := mgr.SetJobSafe(context.Background(), "does-not-exist", true); err == nil {
		t.Fatalf("expected error for missing job")
	}
}

func TestRunJobPrefetchInjectsRunnerContextAndLearnsCommands(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	now := context.Background()
	if err := store.UpsertCronJob(now, db.CronJob{
		ID:           "job-prefetch",
		Schedule:     "0 * * * *",
		Prompt:       "check PR status",
		Transport:    "telegram",
		SessionKey:   "8750063231",
		Active:       true,
		Safe:         true,
		AutoPrefetch: true,
	}); err != nil {
		t.Fatalf("upsert cron job: %v", err)
	}
	if err := store.UpsertCronPrefetchCommands(now, "job-prefetch", []string{"pwd"}); err != nil {
		t.Fatalf("seed prefetch commands: %v", err)
	}

	mgr := NewManager(store)
	seenPrompt := ""
	seenPrefetched := []tools.PrefetchedBashResult{}
	mgr.SetRunner(func(ctx context.Context, transport, sessionKey, prompt string) (RunResult, error) {
		seenPrompt = prompt
		if prefetched, ok := tools.PrefetchedBashResultsFromContext(ctx); ok {
			seenPrefetched = prefetched
		}
		return RunResult{
			Output: "done",
			BashCommands: []string{
				"gh pr view 15072 --json number,title,state",
				"gh pr view 15072 --json number,title,state", // duplicate should dedupe
				"cat file | head -5",                         // has shell control operator, should be rejected for learning
			},
		}, nil
	})
	mgr.runJob("job-prefetch")

	if strings.TrimSpace(seenPrompt) != "check PR status" {
		t.Fatalf("expected original prompt to be passed to runner, got %q", seenPrompt)
	}
	if len(seenPrefetched) != 1 {
		t.Fatalf("expected 1 prefetched command in runner context, got %d (%v)", len(seenPrefetched), seenPrefetched)
	}
	if strings.TrimSpace(seenPrefetched[0].Command) != "pwd" {
		t.Fatalf("expected prefetched command pwd, got %q", seenPrefetched[0].Command)
	}
	if strings.TrimSpace(seenPrefetched[0].FetchedAt) == "" {
		t.Fatalf("expected prefetched command timestamp, got %+v", seenPrefetched[0])
	}
	if strings.TrimSpace(seenPrefetched[0].RunStarted) == "" {
		t.Fatalf("expected prefetch run_started_at timestamp, got %+v", seenPrefetched[0])
	}
	firstRunID := strings.TrimSpace(seenPrefetched[0].RunID)
	if firstRunID == "" {
		t.Fatalf("expected prefetch run_id to be set, got %+v", seenPrefetched[0])
	}
	mgr.runJob("job-prefetch")
	if len(seenPrefetched) < 1 {
		t.Fatalf("expected prefetched commands on second run, got %d (%v)", len(seenPrefetched), seenPrefetched)
	}
	secondRunID := ""
	for _, p := range seenPrefetched {
		if strings.TrimSpace(p.Command) == "pwd" {
			secondRunID = strings.TrimSpace(p.RunID)
			break
		}
	}
	if secondRunID == "" {
		t.Fatalf("expected prefetch run_id on second run, got %+v", seenPrefetched[0])
	}
	if secondRunID == firstRunID {
		t.Fatalf("expected new run_id each run, got identical value %q", secondRunID)
	}
	learned, err := store.ListCronPrefetchCommands(context.Background(), "job-prefetch")
	if err != nil {
		t.Fatalf("list learned prefetch commands: %v", err)
	}
	joined := strings.Join(learned, "\n")
	if !strings.Contains(joined, "gh pr view 15072 --json number,title,state") {
		t.Fatalf("expected learned gh command, got %v", learned)
	}
	if strings.Contains(joined, "cat file | head -5") {
		t.Fatalf("expected shell-control command to be rejected from learning, got %v", learned)
	}
}

func TestListJobPrefetchCommands(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()

	mgr := NewManager(store)
	ctx := context.Background()
	if _, err := mgr.ListJobPrefetchCommands(ctx, "missing"); err == nil {
		t.Fatalf("expected missing job error")
	}

	if err := store.UpsertCronJob(ctx, db.CronJob{
		ID:           "job-prefetch-list",
		Schedule:     "0 * * * *",
		Prompt:       "check",
		Transport:    "telegram",
		SessionKey:   "8750063231",
		Active:       true,
		AutoPrefetch: true,
	}); err != nil {
		t.Fatalf("upsert cron job: %v", err)
	}
	if err := store.UpsertCronPrefetchCommands(ctx, "job-prefetch-list", []string{"pwd", "gh pr view 1 --json number"}); err != nil {
		t.Fatalf("upsert prefetch commands: %v", err)
	}

	commands, err := mgr.ListJobPrefetchCommands(ctx, "job-prefetch-list")
	if err != nil {
		t.Fatalf("ListJobPrefetchCommands() error: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("expected 2 commands, got %d (%v)", len(commands), commands)
	}
}

func TestToToolInfoFormatsTimesInPacific(t *testing.T) {
	last := time.Date(2026, time.January, 15, 20, 0, 0, 0, time.UTC)
	next := time.Date(2026, time.January, 15, 21, 30, 0, 0, time.UTC)
	info := toToolInfo(db.CronJob{
		ID:           "job-tz",
		Schedule:     "0 * * * *",
		Prompt:       "check status",
		Transport:    "telegram",
		SessionKey:   "8750063231",
		Active:       true,
		Safe:         false,
		AutoPrefetch: true,
		LastRunAt:    &last,
		NextRunAt:    &next,
	})
	if info.LastRunAt != "2026-01-15T12:00:00-08:00" {
		t.Fatalf("expected LastRunAt in PST, got %q", info.LastRunAt)
	}
	if info.NextRunAt != "2026-01-15T13:30:00-08:00" {
		t.Fatalf("expected NextRunAt in PST, got %q", info.NextRunAt)
	}
}

func TestScheduleSpecPacificPrefixesTimezone(t *testing.T) {
	spec, err := scheduleSpecPacific("0 9 * * *")
	if err != nil {
		t.Fatalf("scheduleSpecPacific() error: %v", err)
	}
	wantPrefix := "CRON_TZ=America/Los_Angeles "
	if !strings.HasPrefix(spec, wantPrefix) {
		t.Fatalf("expected %q prefix, got %q", wantPrefix, spec)
	}
	if strings.TrimSpace(strings.TrimPrefix(spec, wantPrefix)) != "0 9 * * *" {
		t.Fatalf("unexpected normalized spec %q", spec)
	}
}

func TestScheduleSpecPacificRejectsTimezoneOverride(t *testing.T) {
	for _, spec := range []string{
		"TZ=UTC 0 9 * * *",
		"CRON_TZ=UTC 0 9 * * *",
		"crOn_tz=UTC 0 9 * * *",
	} {
		if _, err := scheduleSpecPacific(spec); err == nil {
			t.Fatalf("expected timezone override rejection for spec %q", spec)
		}
	}
}

func TestParseSchedulePacificUsesPacificIndependentlyOfTimeLocal(t *testing.T) {
	origLocal := time.Local
	t.Cleanup(func() { time.Local = origLocal })
	time.Local = time.FixedZone("UTC+09", 9*60*60)

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parseSchedulePacific(parser, "0 9 * * *")
	if err != nil {
		t.Fatalf("parseSchedulePacific() error: %v", err)
	}
	nowUTC := time.Date(2026, time.April, 14, 14, 30, 0, 0, time.UTC)
	next := sched.Next(nowUTC)
	nextPacific := next.In(util.PacificLocation())
	if nextPacific.Hour() != 9 || nextPacific.Minute() != 0 {
		t.Fatalf("expected next run at 09:00 Pacific, got %s", nextPacific.Format(time.RFC3339))
	}
}

func TestAddJobRejectsTimezonePrefixedSpec(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer store.Close()
	mgr := NewManager(store)
	_, err = mgr.AddJob(context.Background(), tools.CronJobSpec{
		ID:         "job-tz-reject",
		Schedule:   "CRON_TZ=UTC 0 9 * * *",
		Prompt:     "ping",
		Transport:  "repl",
		SessionKey: "default",
	})
	if err == nil {
		t.Fatalf("expected timezone-prefixed spec rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timezone") {
		t.Fatalf("expected timezone-related error, got %v", err)
	}
}

func TestParseSchedulePacificRespectsDSTOffsets(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parseSchedulePacific(parser, "0 9 * * *")
	if err != nil {
		t.Fatalf("parseSchedulePacific() error: %v", err)
	}

	winterNow := time.Date(2026, time.January, 15, 10, 0, 0, 0, time.UTC)
	winterNext := sched.Next(winterNow).In(util.PacificLocation())
	if _, offset := winterNext.Zone(); offset != -8*60*60 {
		t.Fatalf("expected winter PST offset -0800, got %s (%d)", winterNext.Format(time.RFC3339), offset)
	}

	summerNow := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	summerNext := sched.Next(summerNow).In(util.PacificLocation())
	if _, offset := summerNext.Zone(); offset != -7*60*60 {
		t.Fatalf("expected summer PDT offset -0700, got %s (%d)", summerNext.Format(time.RFC3339), offset)
	}
}
