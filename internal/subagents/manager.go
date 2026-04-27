package subagents

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
)

type OutputSinkFunc func(ctx context.Context, transport, sessionKey, content string) error

type Manager struct {
	store *db.Store
	cfg   config.SubagentConfig
	cwd   string

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	signal  chan struct{}
	running map[string]context.CancelFunc
	sink    OutputSinkFunc
	wg      sync.WaitGroup
}

const (
	statusQueued    = "queued"
	statusRunning   = "running"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
	statusCanceled  = "canceled"

	kindGeneric  = "generic"
	kindPRReview = "pr_review"

	resultMaxBytes  = 64 * 1024
	previewMaxRunes = 3400
)

var (
	ownerRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	prShorthand = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)$`)
)

func NewManager(cfg config.SubagentConfig, store *db.Store) *Manager {
	cwd, _ := os.Getwd()
	signalCap := cfg.MaxConcurrent
	if signalCap <= 0 {
		signalCap = 1
	}
	return &Manager{
		store:   store,
		cfg:     cfg,
		cwd:     cwd,
		signal:  make(chan struct{}, signalCap),
		running: map[string]context.CancelFunc{},
	}
}

func (m *Manager) SetOutputSink(fn OutputSinkFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = fn
}

func (m *Manager) Start(ctx context.Context) error {
	if !m.cfg.Enabled {
		return nil
	}
	if m.store == nil {
		return errors.New("subagent store is unavailable")
	}
	if err := os.MkdirAll(filepath.Join(m.cfg.RootDir, "tasks"), 0o755); err != nil {
		return fmt.Errorf("create subagent task dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(m.cfg.RootDir, "worktrees"), 0o755); err != nil {
		return fmt.Errorf("create subagent worktree dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(m.cfg.RootDir, "repos"), 0o755); err != nil {
		return fmt.Errorf("create subagent repo dir: %w", err)
	}
	if err := m.store.MarkRunningSubagentTasksInterrupted(ctx); err != nil {
		return err
	}

	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	m.started = true
	m.cancel = cancel
	workers := m.cfg.MaxConcurrent
	if workers <= 0 {
		workers = 1
	}
	m.mu.Unlock()

	for i := 0; i < workers; i++ {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.worker(runCtx)
		}()
	}
	m.wake()
	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	running := make([]context.CancelFunc, 0, len(m.running))
	for _, fn := range m.running {
		running = append(running, fn)
	}
	m.started = false
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, fn := range running {
		fn()
	}
	m.wg.Wait()
}

func (m *Manager) AddSubagentTask(ctx context.Context, spec tools.SubagentSpec) (tools.SubagentInfo, error) {
	if !m.cfg.Enabled {
		return tools.SubagentInfo{}, errors.New("subagents are disabled")
	}
	prompt := strings.TrimSpace(spec.Prompt)
	if prompt == "" {
		return tools.SubagentInfo{}, errors.New("prompt is required")
	}
	id := strings.TrimSpace(spec.ID)
	if id == "" {
		id = newTaskID()
	}
	kind := strings.TrimSpace(spec.Kind)
	if kind == "" {
		kind = kindGeneric
	}
	deliver := true
	if spec.Deliver != nil {
		deliver = *spec.Deliver
	}
	meta := map[string]interface{}{
		"workdir":          strings.TrimSpace(spec.Workdir),
		"model":            strings.TrimSpace(spec.Model),
		"profile":          strings.TrimSpace(spec.Profile),
		"reasoning_effort": strings.TrimSpace(spec.ReasoningEffort),
		"sandbox":          strings.TrimSpace(spec.Sandbox),
		"timeout_minutes":  spec.TimeoutMinutes,
		"deliver":          deliver,
	}
	metaJSON, _ := json.Marshal(meta)
	task := db.SubagentTask{
		ID:           id,
		Kind:         kind,
		Status:       statusQueued,
		Title:        strings.TrimSpace(spec.Title),
		Prompt:       prompt,
		Transport:    strings.TrimSpace(spec.Transport),
		SessionKey:   strings.TrimSpace(spec.SessionKey),
		Repo:         strings.TrimSpace(spec.Repo),
		PRNumber:     spec.PRNumber,
		PRURL:        strings.TrimSpace(spec.PRURL),
		BaseRef:      strings.TrimSpace(spec.BaseRef),
		HeadRef:      strings.TrimSpace(spec.HeadRef),
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now().UTC(),
	}
	if task.Title == "" {
		task.Title = titleForTask(task)
	}
	if err := m.store.UpsertSubagentTask(ctx, task); err != nil {
		return tools.SubagentInfo{}, err
	}
	m.wake()
	return taskInfo(task), nil
}

func (m *Manager) AddPRReviewTasks(ctx context.Context, spec tools.SubagentPRReviewSpec) ([]tools.SubagentInfo, error) {
	if len(spec.PRs) == 0 {
		return nil, errors.New("at least one PR is required")
	}
	out := make([]tools.SubagentInfo, 0, len(spec.PRs))
	for _, raw := range spec.PRs {
		ref, err := normalizePRRef(strings.TrimSpace(raw), strings.TrimSpace(spec.Repo))
		if err != nil {
			return nil, err
		}
		prompt := buildPRReviewPrompt(ref, spec.Prompt)
		deliver := spec.Deliver
		task, err := m.AddSubagentTask(ctx, tools.SubagentSpec{
			Kind:            kindPRReview,
			Title:           fmt.Sprintf("Review PR %s#%d", fallback(ref.Repo, "repo"), ref.Number),
			Prompt:          prompt,
			Transport:       spec.Transport,
			SessionKey:      spec.SessionKey,
			Repo:            ref.Repo,
			PRNumber:        ref.Number,
			PRURL:           ref.URL,
			BaseRef:         spec.BaseRef,
			Model:           spec.Model,
			Profile:         spec.Profile,
			ReasoningEffort: spec.ReasoningEffort,
			Sandbox:         spec.Sandbox,
			TimeoutMinutes:  spec.TimeoutMinutes,
			Deliver:         deliver,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, nil
}

func (m *Manager) ListSubagentTasks(ctx context.Context, filter tools.SubagentTaskFilter) ([]tools.SubagentInfo, error) {
	tasks, err := m.store.ListSubagentTasks(ctx, db.SubagentTaskFilter{
		Status:     strings.TrimSpace(filter.Status),
		Kind:       strings.TrimSpace(filter.Kind),
		Repo:       strings.TrimSpace(filter.Repo),
		Transport:  strings.TrimSpace(filter.Transport),
		SessionKey: strings.TrimSpace(filter.SessionKey),
		Limit:      filter.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]tools.SubagentInfo, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, taskInfo(task))
	}
	return out, nil
}

func (m *Manager) GetSubagentTask(ctx context.Context, id string) (tools.SubagentInfo, bool, error) {
	task, ok, err := m.store.GetSubagentTask(ctx, strings.TrimSpace(id))
	if err != nil || !ok {
		return tools.SubagentInfo{}, ok, err
	}
	return taskInfo(task), true, nil
}

func (m *Manager) GetSubagentResult(ctx context.Context, id string) (tools.SubagentResult, bool, error) {
	task, ok, err := m.store.GetSubagentTask(ctx, strings.TrimSpace(id))
	if err != nil || !ok {
		return tools.SubagentResult{}, ok, err
	}
	content := ""
	truncated := false
	if strings.TrimSpace(task.ResultPath) != "" {
		content, truncated, err = readLimited(task.ResultPath, resultMaxBytes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return tools.SubagentResult{}, true, err
		}
	}
	return tools.SubagentResult{
		Info:       taskInfo(task),
		Content:    content,
		Truncated:  truncated,
		ResultPath: task.ResultPath,
	}, true, nil
}

func (m *Manager) CancelSubagentTask(ctx context.Context, id string) (tools.SubagentInfo, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return tools.SubagentInfo{}, errors.New("id is required")
	}
	task, ok, err := m.store.GetSubagentTask(ctx, id)
	if err != nil {
		return tools.SubagentInfo{}, err
	}
	if !ok {
		return tools.SubagentInfo{}, fmt.Errorf("subagent task %s not found", id)
	}
	switch task.Status {
	case statusSucceeded, statusFailed, statusCanceled:
		return taskInfo(task), nil
	}
	m.mu.Lock()
	cancel := m.running[id]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := m.store.UpdateSubagentTaskStatus(ctx, id, statusCanceled, "canceled by user"); err != nil {
		return tools.SubagentInfo{}, err
	}
	updated, _, err := m.store.GetSubagentTask(ctx, id)
	if err != nil {
		return tools.SubagentInfo{}, err
	}
	return taskInfo(updated), nil
}

func (m *Manager) worker(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		task, ok, err := m.store.ClaimNextSubagentTask(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-m.signal:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		m.runTask(ctx, task)
	}
}

func (m *Manager) runTask(parent context.Context, task db.SubagentTask) {
	timeout := m.taskTimeout(task)
	runCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	m.mu.Lock()
	m.running[task.ID] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.running, task.ID)
		m.mu.Unlock()
	}()

	taskDir := filepath.Join(m.cfg.RootDir, "tasks", task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		m.failTask(task, fmt.Errorf("create task dir: %w", err), nil)
		return
	}
	task.ResultPath = filepath.Join(taskDir, "result.md")
	task.StdoutPath = filepath.Join(taskDir, "stdout.jsonl")
	task.StderrPath = filepath.Join(taskDir, "stderr.log")
	if err := m.store.UpsertSubagentTask(context.Background(), task); err != nil {
		m.failTask(task, err, nil)
		return
	}

	workdir, err := m.prepareWorkdir(runCtx, &task)
	if err != nil {
		m.failTask(task, err, nil)
		return
	}
	if err := m.store.UpsertSubagentTask(context.Background(), task); err != nil {
		m.failTask(task, err, nil)
		return
	}

	args := m.codexArgs(task, workdir)
	stdout, err := os.Create(task.StdoutPath)
	if err != nil {
		m.failTask(task, fmt.Errorf("create stdout artifact: %w", err), nil)
		return
	}
	defer stdout.Close()
	stderr, err := os.Create(task.StderrPath)
	if err != nil {
		m.failTask(task, fmt.Errorf("create stderr artifact: %w", err), nil)
		return
	}
	defer stderr.Close()

	cmd := exec.CommandContext(runCtx, m.codexBinary(), args...)
	cmd.Stdin = strings.NewReader(task.Prompt)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		m.failTask(task, fmt.Errorf("start codex: %w", err), nil)
		return
	}
	task.PID = cmd.Process.Pid
	_ = m.store.UpsertSubagentTask(context.Background(), task)

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		exitCode = -1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	task.ExitCode = &exitCode
	task.PID = 0
	now := time.Now().UTC()
	task.FinishedAt = &now
	if runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.Canceled) {
			task.Status = statusCanceled
			task.Error = "canceled"
		} else {
			task.Status = statusFailed
			task.Error = fmt.Sprintf("timed out after %s", timeout)
		}
	} else if waitErr != nil {
		task.Status = statusFailed
		task.Error = waitErr.Error()
	} else {
		task.Status = statusSucceeded
		task.Error = ""
	}
	if err := m.store.UpsertSubagentTask(context.Background(), task); err != nil {
		m.failTask(task, err, task.ExitCode)
		return
	}
	m.deliver(task)
}

func (m *Manager) prepareWorkdir(ctx context.Context, task *db.SubagentTask) (string, error) {
	switch task.Kind {
	case kindPRReview:
		return m.preparePRWorktree(ctx, task)
	default:
		return m.prepareGenericWorkdir(ctx, task)
	}
}

func (m *Manager) prepareGenericWorkdir(ctx context.Context, task *db.SubagentTask) (string, error) {
	meta := taskMetadata(task)
	workdir, _ := meta["workdir"].(string)
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		workdir = m.cwd
	}
	if strings.TrimSpace(task.Repo) != "" {
		repoDir, err := m.ensureRepo(ctx, task.Repo)
		if err != nil {
			return "", err
		}
		workdir = repoDir
	}
	if isGitRepo(ctx, workdir) {
		wt := filepath.Join(m.cfg.RootDir, "worktrees", task.ID)
		if err := runCmd(ctx, "", "git", "-C", workdir, "worktree", "add", "--detach", wt, "HEAD"); err != nil {
			return "", err
		}
		task.WorktreePath = wt
		return wt, nil
	}
	task.WorktreePath = workdir
	return workdir, nil
}

func (m *Manager) preparePRWorktree(ctx context.Context, task *db.SubagentTask) (string, error) {
	repo := strings.TrimSpace(task.Repo)
	if repo == "" {
		if fromCWD := repoFromGitRemote(ctx, m.cwd); fromCWD != "" {
			repo = fromCWD
			task.Repo = repo
		}
	}
	if repo == "" {
		return "", errors.New("repo is required for PR review tasks with bare PR numbers")
	}
	repoDir, err := m.ensureRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	meta, err := ghPRView(ctx, repo, task.PRNumber, task.PRURL)
	if err != nil {
		return "", err
	}
	task.PRNumber = meta.Number
	task.PRURL = meta.URL
	task.BaseRef = firstNonEmpty(task.BaseRef, meta.BaseRefName)
	task.HeadRef = meta.HeadRefName
	if task.Title == "" {
		task.Title = fmt.Sprintf("Review PR %s#%d", repo, task.PRNumber)
	}
	remotePRRef := fmt.Sprintf("refs/remotes/origin/pr/%d", task.PRNumber)
	if err := runCmd(ctx, "", "git", "-C", repoDir, "fetch", "origin", fmt.Sprintf("refs/pull/%d/head:%s", task.PRNumber, remotePRRef)); err != nil {
		return "", err
	}
	if task.BaseRef != "" {
		_ = runCmd(ctx, "", "git", "-C", repoDir, "fetch", "origin", fmt.Sprintf("%s:refs/remotes/origin/%s", task.BaseRef, task.BaseRef))
	}
	wt := filepath.Join(m.cfg.RootDir, "worktrees", task.ID)
	if err := runCmd(ctx, "", "git", "-C", repoDir, "worktree", "add", "--detach", wt, remotePRRef); err != nil {
		return "", err
	}
	task.WorktreePath = wt
	return wt, nil
}

func (m *Manager) ensureRepo(ctx context.Context, repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if !ownerRepoRe.MatchString(repo) {
		return "", fmt.Errorf("invalid repo %q, expected owner/name", repo)
	}
	dir := filepath.Join(m.cfg.RootDir, "repos", filepath.FromSlash(repo))
	if isGitRepo(ctx, dir) {
		_ = runCmd(ctx, "", "git", "-C", dir, "fetch", "--all", "--prune")
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := runCmd(ctx, "", "gh", "repo", "clone", repo, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *Manager) codexArgs(task db.SubagentTask, workdir string) []string {
	meta := taskMetadata(&task)
	model, _ := meta["model"].(string)
	profile, _ := meta["profile"].(string)
	reasoningEffort, _ := meta["reasoning_effort"].(string)
	sandbox, _ := meta["sandbox"].(string)
	model = firstNonEmpty(strings.TrimSpace(model), strings.TrimSpace(m.cfg.DefaultModel))
	profile = firstNonEmpty(strings.TrimSpace(profile), strings.TrimSpace(m.cfg.DefaultProfile))
	reasoningEffort = firstNonEmpty(normalizeReasoningEffort(reasoningEffort), normalizeReasoningEffort(m.cfg.DefaultReasoningEffort))
	sandbox = firstNonEmpty(strings.TrimSpace(sandbox), strings.TrimSpace(m.cfg.Sandbox))

	args := []string{"exec", "-C", workdir, "--json", "-o", task.ResultPath}
	if model != "" {
		args = append(args, "--model", model)
	}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	if reasoningEffort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", reasoningEffort))
	}
	if sandbox != "" {
		args = append(args, "--sandbox", sandbox)
	}
	if task.Kind == kindPRReview {
		args = append(args, "review")
		base := strings.TrimSpace(task.BaseRef)
		if base != "" {
			args = append(args, "--base", "origin/"+base)
		}
		args = append(args, "-")
		return args
	}
	args = append(args, "-")
	return args
}

func (m *Manager) taskTimeout(task db.SubagentTask) time.Duration {
	meta := taskMetadata(&task)
	minutes := 0
	switch v := meta["timeout_minutes"].(type) {
	case float64:
		minutes = int(v)
	case int:
		minutes = v
	}
	if minutes <= 0 {
		minutes = m.cfg.DefaultTimeoutMinutes
	}
	if minutes <= 0 {
		minutes = 45
	}
	return time.Duration(minutes) * time.Minute
}

func (m *Manager) codexBinary() string {
	if strings.TrimSpace(m.cfg.CodexBinary) == "" {
		return "codex"
	}
	return strings.TrimSpace(m.cfg.CodexBinary)
}

func normalizeReasoningEffort(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func (m *Manager) failTask(task db.SubagentTask, err error, exitCode *int) {
	now := time.Now().UTC()
	task.Status = statusFailed
	task.PID = 0
	task.Error = err.Error()
	task.ExitCode = exitCode
	task.FinishedAt = &now
	_ = m.store.UpsertSubagentTask(context.Background(), task)
	m.deliver(task)
}

func (m *Manager) deliver(task db.SubagentTask) {
	meta := taskMetadata(&task)
	deliver := true
	if v, ok := meta["deliver"].(bool); ok {
		deliver = v
	}
	if !deliver || strings.TrimSpace(task.Transport) == "" || strings.TrimSpace(task.SessionKey) == "" {
		return
	}
	m.mu.Lock()
	sink := m.sink
	m.mu.Unlock()
	if sink == nil {
		return
	}
	content := formatCompletion(task)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = sink(ctx, task.Transport, task.SessionKey, content)
}

func (m *Manager) wake() {
	n := m.cfg.MaxConcurrent
	if n <= 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		select {
		case m.signal <- struct{}{}:
		default:
			return
		}
	}
}

type prRef struct {
	Repo   string
	Number int
	URL    string
}

func normalizePRRef(raw, defaultRepo string) (prRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return prRef{}, errors.New("empty PR reference")
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		if strings.TrimSpace(defaultRepo) == "" {
			return prRef{}, fmt.Errorf("repo is required for bare PR number %d", n)
		}
		return prRef{Repo: strings.TrimSpace(defaultRepo), Number: n}, nil
	}
	if m := prShorthand.FindStringSubmatch(raw); len(m) == 3 {
		n, _ := strconv.Atoi(m[2])
		return prRef{Repo: m[1], Number: n}, nil
	}
	u, err := url.Parse(raw)
	if err == nil && strings.EqualFold(u.Host, "github.com") {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 4 && parts[2] == "pull" {
			n, convErr := strconv.Atoi(parts[3])
			if convErr == nil && n > 0 {
				return prRef{Repo: parts[0] + "/" + parts[1], Number: n, URL: raw}, nil
			}
		}
	}
	return prRef{}, fmt.Errorf("unsupported PR reference %q", raw)
}

func buildPRReviewPrompt(ref prRef, extra string) string {
	lines := []string{
		"Run a report-only code review for this pull request.",
		"Do not post comments, submit reviews, push branches, or mutate GitHub state.",
		"Focus findings first, ordered by severity, with file/line references where possible.",
		"Use gh only for read-only inspection if needed.",
		fmt.Sprintf("PR: %s#%d", fallback(ref.Repo, "(current repo)"), ref.Number),
	}
	if strings.TrimSpace(ref.URL) != "" {
		lines = append(lines, "URL: "+strings.TrimSpace(ref.URL))
	}
	if strings.TrimSpace(extra) != "" {
		lines = append(lines, "", "Extra instructions:", strings.TrimSpace(extra))
	}
	return strings.Join(lines, "\n")
}

type ghPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	HeadRefOID  string `json:"headRefOid"`
}

func ghPRView(ctx context.Context, repo string, number int, prURL string) (ghPR, error) {
	ref := strings.TrimSpace(prURL)
	if ref == "" && number > 0 {
		ref = strconv.Itoa(number)
	}
	if ref == "" {
		return ghPR{}, errors.New("PR number or URL is required")
	}
	args := []string{"pr", "view", ref, "-R", repo, "--json", "number,title,url,baseRefName,headRefName,headRefOid"}
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		return ghPR{}, commandError("gh", args, err)
	}
	var pr ghPR
	if err := json.Unmarshal(out, &pr); err != nil {
		return ghPR{}, fmt.Errorf("parse gh pr view output: %w", err)
	}
	if pr.Number <= 0 {
		return ghPR{}, errors.New("gh pr view returned no PR number")
	}
	return pr, nil
}

func runCmd(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s failed: %s", name, strings.Join(args, " "), msg)
	}
	return nil
}

func commandError(name string, args []string, err error) error {
	if exitErr, ok := err.(*exec.ExitError); ok {
		msg := strings.TrimSpace(string(exitErr.Stderr))
		if msg != "" {
			return fmt.Errorf("%s %s failed: %s", name, strings.Join(args, " "), msg)
		}
	}
	return fmt.Errorf("%s %s failed: %w", name, strings.Join(args, " "), err)
}

func isGitRepo(ctx context.Context, dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func repoFromGitRemote(ctx context.Context, dir string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return repoFromRemoteURL(strings.TrimSpace(string(out)))
}

func repoFromRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, ".git")
	if strings.HasPrefix(raw, "git@github.com:") {
		return strings.TrimPrefix(raw, "git@github.com:")
	}
	if u, err := url.Parse(raw); err == nil && strings.EqualFold(u.Host, "github.com") {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	return ""
}

func formatCompletion(task db.SubagentTask) string {
	header := fmt.Sprintf("subagent %s: %s\nid: %s", task.Status, fallback(task.Title, task.Kind), task.ID)
	if task.Repo != "" || task.PRNumber > 0 {
		header += fmt.Sprintf("\nrepo: %s\npr: %d", fallback(task.Repo, "-"), task.PRNumber)
	}
	if task.Error != "" {
		header += "\nerror: " + task.Error
	}
	if task.ResultPath != "" {
		header += "\nresult: " + task.ResultPath
	}
	if task.Status != statusSucceeded {
		return header
	}
	content, truncated, err := readLimited(task.ResultPath, resultMaxBytes)
	if err != nil || strings.TrimSpace(content) == "" {
		return header
	}
	preview := truncateRunes(strings.TrimSpace(content), previewMaxRunes)
	if truncated || preview != strings.TrimSpace(content) {
		preview += "\n\n...[truncated; use /agents show " + task.ID + " for the stored result]"
	}
	return header + "\n\n" + preview
}

func taskInfo(task db.SubagentTask) tools.SubagentInfo {
	meta := taskMetadata(&task)
	return tools.SubagentInfo{
		ID:           task.ID,
		Kind:         task.Kind,
		Status:       task.Status,
		Title:        task.Title,
		Transport:    task.Transport,
		SessionKey:   task.SessionKey,
		Repo:         task.Repo,
		PRNumber:     task.PRNumber,
		PRURL:        task.PRURL,
		BaseRef:      task.BaseRef,
		HeadRef:      task.HeadRef,
		WorktreePath: task.WorktreePath,
		ResultPath:   task.ResultPath,
		StdoutPath:   task.StdoutPath,
		StderrPath:   task.StderrPath,
		Metadata:     meta,
		PID:          task.PID,
		ExitCode:     task.ExitCode,
		Error:        task.Error,
		CreatedAt:    formatTime(task.CreatedAt),
		StartedAt:    formatTimePtr(task.StartedAt),
		FinishedAt:   formatTimePtr(task.FinishedAt),
		UpdatedAt:    formatTime(task.UpdatedAt),
	}
}

func taskMetadata(task *db.SubagentTask) map[string]interface{} {
	out := map[string]interface{}{}
	if strings.TrimSpace(task.MetadataJSON) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(task.MetadataJSON), &out)
	return out
}

func titleForTask(task db.SubagentTask) string {
	if task.Kind == kindPRReview && (task.Repo != "" || task.PRNumber > 0) {
		return fmt.Sprintf("Review PR %s#%d", fallback(task.Repo, "repo"), task.PRNumber)
	}
	return "Codex background task"
}

func readLimited(path string, max int) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	buf := make([]byte, max+1)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	truncated := n > max
	if truncated {
		n = max
	}
	return string(buf[:n]), truncated, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}

func firstNonEmpty(items ...string) string {
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			return strings.TrimSpace(item)
		}
	}
	return ""
}

func fallback(v, fb string) string {
	if strings.TrimSpace(v) == "" {
		return fb
	}
	return strings.TrimSpace(v)
}

func newTaskID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("agent-%d", time.Now().UTC().UnixNano())
	}
	return fmt.Sprintf("agent-%d-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(b[:]))
}
