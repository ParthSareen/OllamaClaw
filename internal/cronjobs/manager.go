package cronjobs

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/parth/ollamaclaw/internal/db"
	"github.com/parth/ollamaclaw/internal/tools"
	"github.com/robfig/cron/v3"
)

type RunnerFunc func(ctx context.Context, transport, sessionKey, prompt string) (string, error)
type OutputSinkFunc func(ctx context.Context, transport, sessionKey, content string) error

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

func NewManager(store *db.Store) *Manager {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	c := cron.New(cron.WithParser(parser))
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
	job := db.CronJob{ID: id, Schedule: schedule, Prompt: prompt, Transport: transport, SessionKey: sessionKey, Active: true, CreatedAt: now, UpdatedAt: now}
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

func (m *Manager) scheduleLocked(ctx context.Context, job db.CronJob) error {
	schedule, err := m.parser.Parse(job.Schedule)
	if err != nil {
		return err
	}
	next := schedule.Next(time.Now().UTC())
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
	ctx := context.Background()
	job, ok, err := m.store.GetCronJob(ctx, jobID)
	if err != nil || !ok || !job.Active {
		return
	}

	m.mu.Lock()
	runner := m.runner
	sink := m.sink
	m.mu.Unlock()
	if runner == nil {
		_ = m.store.UpdateCronRun(ctx, job.ID, nil, job.NextRunAt, "no runner configured")
		return
	}

	now := time.Now().UTC()
	resp, runErr := runner(ctx, job.Transport, job.SessionKey, job.Prompt)
	spec, parseErr := m.parser.Parse(job.Schedule)
	var next *time.Time
	if parseErr == nil {
		n := spec.Next(now)
		next = &n
	}
	if runErr != nil {
		_ = m.store.UpdateCronRun(ctx, job.ID, &now, next, runErr.Error())
		return
	}
	_ = m.store.UpdateCronRun(ctx, job.ID, &now, next, "")
	if sink != nil && strings.TrimSpace(resp) != "" {
		_ = sink(ctx, job.Transport, job.SessionKey, resp)
	}
}

func toToolInfo(j db.CronJob) tools.CronJobInfo {
	info := tools.CronJobInfo{
		ID:         j.ID,
		Schedule:   j.Schedule,
		Prompt:     j.Prompt,
		Transport:  j.Transport,
		SessionKey: j.SessionKey,
		Active:     j.Active,
		LastError:  j.LastError,
	}
	if j.LastRunAt != nil {
		info.LastRunAt = j.LastRunAt.UTC().Format(time.RFC3339)
	}
	if j.NextRunAt != nil {
		info.NextRunAt = j.NextRunAt.UTC().Format(time.RFC3339)
	}
	return info
}
