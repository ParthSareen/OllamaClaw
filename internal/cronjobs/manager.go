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
	Compacted    bool
}

type RunnerFunc func(ctx context.Context, transport, sessionKey, prompt string) (RunResult, error)
type OutputSinkFunc func(ctx context.Context, transport, sessionKey, content string) error

type safeReminderBashApprover struct{}

func (safeReminderBashApprover) ApproveBashCommand(context.Context, tools.BashApprovalRequest) error {
	return nil
}

type Manager struct {
	store *db.Store

	mu      sync.Mutex
	entries map[string]cron.EntryID
	running map[string]struct{}
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
		running: map[string]struct{}{},
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

	reminders, err := m.store.ListReminderJobs(ctx, true)
	if err != nil {
		return err
	}
	nowPacific := time.Now().In(util.PacificLocation())
	dueOnStartup := make([]string, 0, len(reminders))
	for _, r := range reminders {
		if r.NextRunAt != nil && !r.NextRunAt.After(nowPacific) {
			dueOnStartup = append(dueOnStartup, r.ID)
		}
		if err := m.scheduleLocked(ctx, r); err != nil {
			_ = m.store.UpdateReminderRun(ctx, r.ID, nil, nil, "schedule error: "+err.Error())
		}
	}
	m.c.Start()
	if len(dueOnStartup) > 0 {
		go m.runStartupCatchUp(dueOnStartup)
	}
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

func (m *Manager) AddReminder(ctx context.Context, spec tools.ReminderSpec) (tools.ReminderInfo, error) {
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		id = fmt.Sprintf("reminder-%d", time.Now().UTC().UnixNano())
	}
	prompt := strings.TrimSpace(spec.Prompt)
	transport := strings.TrimSpace(spec.Transport)
	sessionKey := strings.TrimSpace(spec.SessionKey)
	if prompt == "" || transport == "" || sessionKey == "" {
		return tools.ReminderInfo{}, fmt.Errorf("id(optional), prompt, transport, and session_key are required")
	}
	compiled, err := CompileReminderSpecPacific(spec, time.Now())
	if err != nil {
		return tools.ReminderInfo{}, err
	}
	if _, err := parseSchedulePacific(m.parser, compiled.CompiledSchedule); err != nil {
		return tools.ReminderInfo{}, fmt.Errorf("compiled reminder schedule is invalid: %w", err)
	}

	now := time.Now().UTC()
	autoPrefetch := true
	if spec.AutoPrefetch != nil {
		autoPrefetch = *spec.AutoPrefetch
	}
	job := db.ReminderJob{
		ID:               id,
		Schedule:         compiled.CompiledSchedule,
		Prompt:           prompt,
		Transport:        transport,
		SessionKey:       sessionKey,
		Active:           true,
		Safe:             spec.Safe,
		AutoPrefetch:     autoPrefetch,
		ReminderMode:     compiled.Mode,
		ReminderSpecJSON: compiled.NormalizedSpecJSON,
		OnceFireAt:       compiled.OnceFireAt,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := m.store.UpsertReminderJob(ctx, job); err != nil {
		return tools.ReminderInfo{}, err
	}
	if err := m.scheduleLocked(ctx, job); err != nil {
		return tools.ReminderInfo{}, err
	}
	stored, _, err := m.store.GetReminderJob(ctx, id)
	if err != nil {
		return tools.ReminderInfo{}, err
	}
	return toToolInfo(stored), nil
}

func (m *Manager) ListReminders(ctx context.Context, activeOnly bool) ([]tools.ReminderInfo, error) {
	reminders, err := m.store.ListReminderJobs(ctx, activeOnly)
	if err != nil {
		return nil, err
	}
	sort.Slice(reminders, func(i, j int) bool { return reminders[i].ID < reminders[j].ID })
	out := make([]tools.ReminderInfo, 0, len(reminders))
	for _, j := range reminders {
		out = append(out, toToolInfo(j))
	}
	return out, nil
}

func (m *Manager) RemoveReminder(ctx context.Context, id string) error {
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
	return m.store.DeleteReminderJob(ctx, id)
}

func (m *Manager) SetReminderSafe(ctx context.Context, id string, safe bool) (tools.ReminderInfo, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return tools.ReminderInfo{}, fmt.Errorf("id is required")
	}
	job, ok, err := m.store.GetReminderJob(ctx, id)
	if err != nil {
		return tools.ReminderInfo{}, err
	}
	if !ok {
		return tools.ReminderInfo{}, fmt.Errorf("reminder %s not found", id)
	}
	job.Safe = safe
	if err := m.store.UpsertReminderJob(ctx, job); err != nil {
		return tools.ReminderInfo{}, err
	}
	updated, ok, err := m.store.GetReminderJob(ctx, id)
	if err != nil {
		return tools.ReminderInfo{}, err
	}
	if !ok {
		return tools.ReminderInfo{}, fmt.Errorf("reminder %s not found after update", id)
	}
	return toToolInfo(updated), nil
}

func (m *Manager) ListReminderPrefetchCommands(ctx context.Context, id string) ([]string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	_, ok, err := m.store.GetReminderJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("reminder %s not found", id)
	}
	return m.store.ListReminderPrefetchCommands(ctx, id)
}

func (m *Manager) scheduleLocked(ctx context.Context, job db.ReminderJob) error {
	schedule, err := parseSchedulePacific(m.parser, job.Schedule)
	if err != nil {
		return err
	}
	nowPacific := time.Now().In(util.PacificLocation())
	next := schedule.Next(nowPacific)
	job.NextRunAt = &next
	if err := m.store.UpsertReminderJob(ctx, job); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if eid, ok := m.entries[job.ID]; ok {
		m.c.Remove(eid)
		delete(m.entries, job.ID)
	}
	execSpec, err := scheduleSpecPacific(job.Schedule)
	if err != nil {
		return err
	}
	eid, err := m.c.AddFunc(execSpec, func() { m.runJob(job.ID) })
	if err != nil {
		return err
	}
	m.entries[job.ID] = eid
	return nil
}

func (m *Manager) runJob(jobID string) {
	baseCtx := context.Background()
	job, ok, err := m.store.GetReminderJob(baseCtx, jobID)
	if err != nil || !ok || !job.Active {
		return
	}
	if !m.beginJobRun(job.ID) {
		return
	}
	defer m.endJobRun(job.ID)

	m.mu.Lock()
	runner := m.runner
	sink := m.sink
	m.mu.Unlock()
	if runner == nil {
		_ = m.store.UpdateReminderRun(baseCtx, job.ID, nil, job.NextRunAt, "no runner configured")
		return
	}

	now := time.Now().In(util.PacificLocation())
	runCtx := baseCtx
	if job.Safe {
		runCtx = tools.WithBashApprover(runCtx, safeReminderBashApprover{})
	}
	prefetchCommands, prefetchErr := m.store.ListReminderPrefetchCommands(baseCtx, job.ID)
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
	spec, parseErr := parseSchedulePacific(m.parser, job.Schedule)
	var next *time.Time
	if parseErr == nil {
		n := spec.Next(now)
		next = &n
	}
	isOnce := strings.EqualFold(strings.TrimSpace(job.ReminderMode), "once")
	if runErr != nil {
		if isOnce {
			next = nil
		}
		_ = m.store.UpdateReminderRun(baseCtx, job.ID, &now, next, runErr.Error())
		if isOnce {
			_ = m.store.SetReminderActive(baseCtx, job.ID, false)
			m.unschedule(job.ID)
		}
		return
	}
	if job.AutoPrefetch {
		m.learnPrefetchCommands(baseCtx, job.ID, res.BashCommands)
	}
	if isOnce {
		next = nil
	}
	_ = m.store.UpdateReminderRun(baseCtx, job.ID, &now, next, "")
	if isOnce {
		_ = m.store.SetReminderActive(baseCtx, job.ID, false)
		m.unschedule(job.ID)
	}
	if sink != nil && strings.TrimSpace(res.Output) != "" {
		_ = sink(baseCtx, job.Transport, job.SessionKey, res.Output)
	}
}

func (m *Manager) runStartupCatchUp(ids []string) {
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		m.runJob(id)
	}
}

func (m *Manager) unschedule(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if eid, ok := m.entries[id]; ok {
		m.c.Remove(eid)
		delete(m.entries, id)
	}
}

func (m *Manager) beginJobRun(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		m.running = map[string]struct{}{}
	}
	if _, exists := m.running[id]; exists {
		return false
	}
	m.running[id] = struct{}{}
	return true
}

func (m *Manager) endJobRun(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, id)
}

func parseSchedulePacific(parser cron.Parser, schedule string) (cron.Schedule, error) {
	spec, err := scheduleSpecPacific(schedule)
	if err != nil {
		return nil, err
	}
	return parser.Parse(spec)
}

func scheduleSpecPacific(schedule string) (string, error) {
	spec := strings.TrimSpace(schedule)
	if spec == "" {
		return "", fmt.Errorf("schedule is required")
	}
	upper := strings.ToUpper(spec)
	if strings.HasPrefix(upper, "TZ=") || strings.HasPrefix(upper, "CRON_TZ=") {
		return "", fmt.Errorf("timezone prefix is not supported; schedules run in %s", util.PacificTimezoneName)
	}
	return fmt.Sprintf("CRON_TZ=%s %s", util.PacificTimezoneName, spec), nil
}

func toToolInfo(j db.ReminderJob) tools.ReminderInfo {
	mode := strings.TrimSpace(j.ReminderMode)
	if mode == "" {
		mode = "legacy_cron"
	}
	spec := map[string]interface{}{}
	if strings.TrimSpace(j.ReminderSpecJSON) != "" {
		_ = json.Unmarshal([]byte(j.ReminderSpecJSON), &spec)
	}
	if len(spec) == 0 && mode == "legacy_cron" {
		spec["schedule"] = j.Schedule
	}
	info := tools.ReminderInfo{
		ID:               j.ID,
		Mode:             mode,
		CompiledSchedule: j.Schedule,
		Prompt:           j.Prompt,
		Transport:        j.Transport,
		SessionKey:       j.SessionKey,
		Active:           j.Active,
		Safe:             j.Safe,
		AutoPrefetch:     j.AutoPrefetch,
		Spec:             spec,
		LastError:        j.LastError,
	}
	if j.OnceFireAt != nil {
		info.OnceFireAt = util.FormatPacificRFC3339(*j.OnceFireAt)
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
	existing, err := m.store.ListReminderPrefetchCommands(ctx, jobID)
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
	_ = m.store.UpsertReminderPrefetchCommands(ctx, jobID, additions)
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
