package cronjobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
	"github.com/ParthSareen/OllamaClaw/internal/util"
	"github.com/robfig/cron/v3"
)

type RunResult struct {
	Output       string
	BashCommands []string
}

type RunnerFunc func(ctx context.Context, transport, sessionKey, prompt string) (RunResult, error)
type OutputSinkFunc func(ctx context.Context, transport, sessionKey, content string) error

type safeCronBashApprover struct{}

func (safeCronBashApprover) ApproveBashCommand(context.Context, tools.BashApprovalRequest) error {
	return nil
}

type Manager struct {
	store *db.Store

	mu      sync.Mutex
	entries map[string]cron.EntryID
	parser  cron.Parser
	c       *cron.Cron
	started bool

	runner RunnerFunc
	sink   OutputSinkFunc
}

const (
	prefetchCommandTimeout   = 20 * time.Second
	prefetchOutputMaxBytes   = 4000
	maxLearnedPrefetchPerRun = 4
	maxPrefetchCommands      = 8
)

type prefetchCommandResult struct {
	Command    string
	RunID      string
	RunStarted string
	FetchedAt  string
	ExitCode   int
	Stdout     string
	Stderr     string
	DurationMs int64
}

func NewManager(store *db.Store) *Manager {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	c := cron.New(cron.WithParser(parser), cron.WithLocation(util.PacificLocation()))
	return &Manager{
		store:   store,
		entries: map[string]cron.EntryID{},
		parser:  parser,
		c:       c,
	}
}

func (m *Manager) SetRunner(fn RunnerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runner = fn
}

func (m *Manager) SetOutputSink(fn OutputSinkFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = fn
}

func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.mu.Unlock()

	jobs, err := m.store.ListCronJobs(ctx, true)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err := m.scheduleLocked(ctx, j); err != nil {
			_ = m.store.UpdateCronRun(ctx, j.ID, nil, nil, "schedule error: "+err.Error())
		}
	}
	m.c.Start()
	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return
	}
	m.started = false
	m.mu.Unlock()
	ctx := m.c.Stop()
	<-ctx.Done()
}

func (m *Manager) AddJob(ctx context.Context, spec tools.CronJobSpec) (tools.CronJobInfo, error) {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		id = fmt.Sprintf("job-%d", time.Now().UTC().UnixNano())
	}
	schedule := strings.TrimSpace(spec.Schedule)
	prompt := strings.TrimSpace(spec.Prompt)
	transport := strings.TrimSpace(spec.Transport)
	sessionKey := strings.TrimSpace(spec.SessionKey)
	if schedule == "" || prompt == "" || transport == "" || sessionKey == "" {
		return tools.CronJobInfo{}, fmt.Errorf("id(optional), schedule, prompt, transport, and session_key are required")
	}
	if _, err := m.parser.Parse(schedule); err != nil {
		return tools.CronJobInfo{}, fmt.Errorf("invalid cron schedule: %w", err)
	}

	now := time.Now().UTC()
	autoPrefetch := true
	if spec.AutoPrefetch != nil {
		autoPrefetch = *spec.AutoPrefetch
	}
	job := db.CronJob{
		ID:           id,
		Schedule:     schedule,
		Prompt:       prompt,
		Transport:    transport,
		SessionKey:   sessionKey,
		Active:       true,
		Safe:         spec.Safe,
		AutoPrefetch: autoPrefetch,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := m.store.UpsertCronJob(ctx, job); err != nil {
		return tools.CronJobInfo{}, err
	}
	if err := m.scheduleLocked(ctx, job); err != nil {
		return tools.CronJobInfo{}, err
	}
	stored, _, err := m.store.GetCronJob(ctx, id)
	if err != nil {
		return tools.CronJobInfo{}, err
	}
	return toToolInfo(stored), nil
}

func (m *Manager) ListJobs(ctx context.Context, activeOnly bool) ([]tools.CronJobInfo, error) {
	jobs, err := m.store.ListCronJobs(ctx, activeOnly)
	if err != nil {
		return nil, err
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	out := make([]tools.CronJobInfo, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toToolInfo(j))
	}
	return out, nil
}

func (m *Manager) RemoveJob(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	m.mu.Lock()
	if eid, ok := m.entries[id]; ok {
		m.c.Remove(eid)
		delete(m.entries, id)
	}
	m.mu.Unlock()
	return m.store.DeleteCronJob(ctx, id)
}

func (m *Manager) SetJobSafe(ctx context.Context, id string, safe bool) (tools.CronJobInfo, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return tools.CronJobInfo{}, fmt.Errorf("id is required")
	}
	job, ok, err := m.store.GetCronJob(ctx, id)
	if err != nil {
		return tools.CronJobInfo{}, err
	}
	if !ok {
		return tools.CronJobInfo{}, fmt.Errorf("cron job %s not found", id)
	}
	job.Safe = safe
	if err := m.store.UpsertCronJob(ctx, job); err != nil {
		return tools.CronJobInfo{}, err
	}
	updated, ok, err := m.store.GetCronJob(ctx, id)
	if err != nil {
		return tools.CronJobInfo{}, err
	}
	if !ok {
		return tools.CronJobInfo{}, fmt.Errorf("cron job %s not found after update", id)
	}
	return toToolInfo(updated), nil
}

func (m *Manager) ListJobPrefetchCommands(ctx context.Context, id string) ([]string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	_, ok, err := m.store.GetCronJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("cron job %s not found", id)
	}
	return m.store.ListCronPrefetchCommands(ctx, id)
}

func (m *Manager) scheduleLocked(ctx context.Context, job db.CronJob) error {
	schedule, err := m.parser.Parse(job.Schedule)
	if err != nil {
		return err
	}
	nowPacific := time.Now().In(util.PacificLocation())
	next := schedule.Next(nowPacific)
	job.NextRunAt = &next
	if err := m.store.UpsertCronJob(ctx, job); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if eid, ok := m.entries[job.ID]; ok {
		m.c.Remove(eid)
		delete(m.entries, job.ID)
	}
	eid, err := m.c.AddFunc(job.Schedule, func() { m.runJob(job.ID) })
	if err != nil {
		return err
	}
	m.entries[job.ID] = eid
	return nil
}

func (m *Manager) runJob(jobID string) {
	baseCtx := context.Background()
	job, ok, err := m.store.GetCronJob(baseCtx, jobID)
	if err != nil || !ok || !job.Active {
		return
	}

	m.mu.Lock()
	runner := m.runner
	sink := m.sink
	m.mu.Unlock()
	if runner == nil {
		_ = m.store.UpdateCronRun(baseCtx, job.ID, nil, job.NextRunAt, "no runner configured")
		return
	}

	now := time.Now().In(util.PacificLocation())
	runCtx := baseCtx
	if job.Safe {
		runCtx = tools.WithBashApprover(runCtx, safeCronBashApprover{})
	}
	prefetchCommands, prefetchErr := m.store.ListCronPrefetchCommands(baseCtx, job.ID)
	if prefetchErr != nil {
		prefetchCommands = nil
	}
	prefetched := []prefetchCommandResult{}
	if job.AutoPrefetch && len(prefetchCommands) > 0 {
		runID := newPrefetchRunID()
		prefetched = executePrefetchCommands(runCtx, now, runID, prefetchCommands)
	}
	if len(prefetched) > 0 {
		runCtx = tools.WithPrefetchedBashResults(runCtx, toToolPrefetchedBashResults(prefetched))
	}
	effectivePrompt := job.Prompt

	res, runErr := runner(runCtx, job.Transport, job.SessionKey, effectivePrompt)
	spec, parseErr := m.parser.Parse(job.Schedule)
	var next *time.Time
	if parseErr == nil {
		n := spec.Next(now)
		next = &n
	}
	if runErr != nil {
		_ = m.store.UpdateCronRun(baseCtx, job.ID, &now, next, runErr.Error())
		return
	}
	if job.AutoPrefetch {
		m.learnPrefetchCommands(baseCtx, job.ID, res.BashCommands)
	}
	_ = m.store.UpdateCronRun(baseCtx, job.ID, &now, next, "")
	if sink != nil && strings.TrimSpace(res.Output) != "" {
		_ = sink(baseCtx, job.Transport, job.SessionKey, res.Output)
	}
}

func toToolInfo(j db.CronJob) tools.CronJobInfo {
	info := tools.CronJobInfo{
		ID:           j.ID,
		Schedule:     j.Schedule,
		Prompt:       j.Prompt,
		Transport:    j.Transport,
		SessionKey:   j.SessionKey,
		Active:       j.Active,
		Safe:         j.Safe,
		AutoPrefetch: j.AutoPrefetch,
		LastError:    j.LastError,
	}
	if j.LastRunAt != nil {
		info.LastRunAt = util.FormatPacificRFC3339(*j.LastRunAt)
	}
	if j.NextRunAt != nil {
		info.NextRunAt = util.FormatPacificRFC3339(*j.NextRunAt)
	}
	return info
}

func executePrefetchCommands(ctx context.Context, runStarted time.Time, runID string, commands []string) []prefetchCommandResult {
	out := make([]prefetchCommandResult, 0, len(commands))
	runStarted = runStarted.In(util.PacificLocation())
	runStartedText := util.FormatPacificRFC3339(runStarted)
	for _, command := range commands {
		command = normalizeCronCommand(command)
		if command == "" {
			continue
		}
		cmdCtx, cancel := context.WithTimeout(ctx, prefetchCommandTimeout)
		startedAt := time.Now().In(util.PacificLocation())
		c := exec.CommandContext(cmdCtx, "/bin/bash", "-lc", command)
		stdout, err := c.Output()
		stderr := ""
		exitCode := 0
		if err != nil {
			if ee := (&exec.ExitError{}); errors.As(err, &ee) {
				exitCode = ee.ExitCode()
				stderr = string(ee.Stderr)
			} else {
				exitCode = -1
				stderr = err.Error()
			}
		}
		cancel()
		if cmdCtx.Err() == context.DeadlineExceeded {
			exitCode = -1
			stderr = strings.TrimSpace(stderr + "\ncommand timed out")
		}
		out = append(out, prefetchCommandResult{
			Command:    command,
			RunID:      runID,
			RunStarted: runStartedText,
			FetchedAt:  util.FormatPacificRFC3339(startedAt),
			ExitCode:   exitCode,
			Stdout:     truncatePrefetch(string(stdout)),
			Stderr:     truncatePrefetch(stderr),
			DurationMs: time.Since(startedAt).Milliseconds(),
		})
	}
	return out
}

func (m *Manager) learnPrefetchCommands(ctx context.Context, jobID string, commands []string) {
	if len(commands) == 0 {
		return
	}
	existing, err := m.store.ListCronPrefetchCommands(ctx, jobID)
	if err != nil {
		return
	}
	known := make(map[string]struct{}, len(existing))
	for _, command := range existing {
		known[normalizeCronCommand(command)] = struct{}{}
	}
	additions := make([]string, 0, maxLearnedPrefetchPerRun)
	for _, raw := range commands {
		if len(known)+len(additions) >= maxPrefetchCommands {
			break
		}
		command := normalizeCronCommand(raw)
		if !isLearnablePrefetchCommand(command) {
			continue
		}
		if _, ok := known[command]; ok {
			continue
		}
		known[command] = struct{}{}
		additions = append(additions, command)
		if len(additions) >= maxLearnedPrefetchPerRun {
			break
		}
	}
	if len(additions) == 0 {
		return
	}
	_ = m.store.UpsertCronPrefetchCommands(ctx, jobID, additions)
}

func normalizeCronCommand(command string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(command)), " ")
}

func isLearnablePrefetchCommand(command string) bool {
	if command == "" {
		return false
	}
	lower := strings.ToLower(command)
	if strings.ContainsAny(lower, ";&|><`$") {
		return false
	}
	if strings.Contains(lower, "ollamaclaw") || strings.Contains(lower, "launch.lock") {
		return false
	}
	fields := strings.Fields(lower)
	if len(fields) == 0 {
		return false
	}
	head := fields[0]
	switch head {
	case "gh", "ls", "pwd", "cat", "head", "tail", "stat", "wc", "find", "grep", "ps", "ollama":
		return true
	case "git":
		if len(fields) < 2 {
			return false
		}
		switch fields[1] {
		case "status", "diff", "show", "log", "rev-parse", "branch", "remote":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func truncatePrefetch(v string) string {
	v = strings.TrimSpace(v)
	if len(v) <= prefetchOutputMaxBytes {
		return v
	}
	return v[:prefetchOutputMaxBytes] + "\n...[truncated]"
}

func BashCommandsFromTrace(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, item := range raw {
		command := normalizeCronCommand(item)
		if command == "" {
			continue
		}
		if _, ok := seen[command]; ok {
			continue
		}
		seen[command] = struct{}{}
		out = append(out, command)
	}
	return out
}

func BashCommandsFromToolTraceJSON(trace []json.RawMessage) []string {
	out := make([]string, 0, len(trace))
	for _, item := range trace {
		var payload map[string]interface{}
		if err := json.Unmarshal(item, &payload); err != nil {
			continue
		}
		command, _ := payload["command"].(string)
		command = normalizeCronCommand(command)
		if command == "" {
			continue
		}
		out = append(out, command)
	}
	return BashCommandsFromTrace(out)
}

func toToolPrefetchedBashResults(results []prefetchCommandResult) []tools.PrefetchedBashResult {
	if len(results) == 0 {
		return nil
	}
	out := make([]tools.PrefetchedBashResult, 0, len(results))
	for _, result := range results {
		out = append(out, tools.PrefetchedBashResult{
			Command:    result.Command,
			RunID:      result.RunID,
			RunStarted: result.RunStarted,
			FetchedAt:  result.FetchedAt,
			ExitCode:   result.ExitCode,
			Stdout:     result.Stdout,
			Stderr:     result.Stderr,
			DurationMs: result.DurationMs,
		})
	}
	return out
}

func newPrefetchRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	}
	return fmt.Sprintf("run-%d-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(b[:]))
}
