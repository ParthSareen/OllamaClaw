package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/ollama"
)

type Executor func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error)

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Execute     Executor
	Source      string
	PluginID    string
	TimeoutSec  int
}

type CronJobSpec struct {
	ID         string
	Schedule   string
	Prompt     string
	Transport  string
	SessionKey string
}

type CronJobInfo struct {
	ID         string `json:"id"`
	Schedule   string `json:"schedule"`
	Prompt     string `json:"prompt"`
	Transport  string `json:"transport"`
	SessionKey string `json:"session_key"`
	Active     bool   `json:"active"`
	LastRunAt  string `json:"last_run_at,omitempty"`
	NextRunAt  string `json:"next_run_at,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

type CronController interface {
	AddJob(ctx context.Context, spec CronJobSpec) (CronJobInfo, error)
	ListJobs(ctx context.Context, activeOnly bool) ([]CronJobInfo, error)
	RemoveJob(ctx context.Context, id string) error
}

type BuiltinsConfig struct {
	ToolOutputMaxBytes int
	BashTimeoutSec     int
	LogPath            string
	Cron               CronController
}

type sessionContextKey struct{}

type SessionInfo struct {
	Transport  string
	SessionKey string
}

func WithSessionInfo(ctx context.Context, transport, sessionKey string) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, SessionInfo{Transport: transport, SessionKey: sessionKey})
}

func SessionInfoFromContext(ctx context.Context) (SessionInfo, bool) {
	v := ctx.Value(sessionContextKey{})
	if v == nil {
		return SessionInfo{}, false
	}
	info, ok := v.(SessionInfo)
	return info, ok
}

func BuiltinTools(cfg BuiltinsConfig, client *ollama.Client) []Tool {
	out := []Tool{
		{
			Name:        "bash",
			Description: "Execute a shell command and return exit code, stdout, and stderr",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "Shell command to execute"},
    "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 600}
  },
  "required": ["command"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				cmdVal, ok := args["command"].(string)
				if !ok || strings.TrimSpace(cmdVal) == "" {
					return nil, errors.New("command is required")
				}
				if err := guardTelegramBashCommand(ctx, cmdVal); err != nil {
					return nil, err
				}
				timeout := cfg.BashTimeoutSec
				if v, ok := asInt(args["timeout_seconds"]); ok && v > 0 && v <= 600 {
					timeout = v
				}
				if timeout <= 0 {
					timeout = 120
				}
				ctxTimeout, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctxTimeout, "/bin/bash", "-lc", cmdVal)
				stdout, err := cmd.Output()
				stderr := ""
				exitCode := 0
				if err != nil {
					if ee := (&exec.ExitError{}); errors.As(err, &ee) {
						exitCode = ee.ExitCode()
						stderr = string(ee.Stderr)
					} else {
						return map[string]any{"exit_code": -1, "stdout": "", "stderr": err.Error()}, nil
					}
				}
				res := map[string]any{
					"exit_code": exitCode,
					"stdout":    truncate(string(stdout), cfg.ToolOutputMaxBytes),
					"stderr":    truncate(stderr, cfg.ToolOutputMaxBytes),
				}
				if ctxTimeout.Err() == context.DeadlineExceeded {
					res["stderr"] = truncate(res["stderr"].(string)+"\ncommand timed out", cfg.ToolOutputMaxBytes)
					res["exit_code"] = -1
				}
				return res, nil
			},
			Source: "builtin",
		},
		{
			Name:        "read_file",
			Description: "Read a file from the local filesystem",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Path to file"}
  },
  "required": ["path"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				_ = ctx
				p, ok := args["path"].(string)
				if !ok || strings.TrimSpace(p) == "" {
					return nil, errors.New("path is required")
				}
				p = expandPath(p)
				b, err := os.ReadFile(p)
				if err != nil {
					return nil, err
				}
				return map[string]any{"path": p, "content": truncate(string(b), cfg.ToolOutputMaxBytes)}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "write_file",
			Description: "Write content to a file on the local filesystem",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Path to file"},
    "content": {"type": "string", "description": "Content to write"},
    "create_dirs": {"type": "boolean", "description": "Create parent directories if missing"}
  },
  "required": ["path", "content"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				_ = ctx
				p, ok := args["path"].(string)
				if !ok || strings.TrimSpace(p) == "" {
					return nil, errors.New("path is required")
				}
				content, ok := args["content"].(string)
				if !ok {
					return nil, errors.New("content must be a string")
				}
				createDirs := false
				if v, ok := args["create_dirs"].(bool); ok {
					createDirs = v
				}
				p = expandPath(p)
				if createDirs {
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						return nil, err
					}
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					return nil, err
				}
				return map[string]any{"path": p, "bytes_written": len(content)}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "read_logs",
			Description: "Read OllamaClaw runtime logs for self-debugging",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "lines": {"type": "integer", "minimum": 1, "maximum": 5000, "description": "How many recent matching lines to return"},
    "contains": {"type": "string", "description": "Optional substring filter"}
  }
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				_ = ctx
				logPath := strings.TrimSpace(cfg.LogPath)
				if logPath == "" {
					return nil, errors.New("log path is not configured")
				}
				lines := 200
				if v, ok := asInt(args["lines"]); ok && v > 0 {
					if v > 5000 {
						v = 5000
					}
					lines = v
				}
				contains := ""
				if v, ok := args["contains"].(string); ok {
					contains = strings.TrimSpace(v)
				}

				f, err := os.Open(logPath)
				if errors.Is(err, os.ErrNotExist) {
					return map[string]interface{}{
						"path":           logPath,
						"total_lines":    0,
						"selected_lines": 0,
						"content":        "",
					}, nil
				}
				if err != nil {
					return nil, err
				}
				defer f.Close()

				all := make([]string, 0, 512)
				scanner := bufio.NewScanner(f)
				scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
				for scanner.Scan() {
					line := scanner.Text()
					if contains != "" && !strings.Contains(line, contains) {
						continue
					}
					all = append(all, line)
				}
				if err := scanner.Err(); err != nil {
					return nil, err
				}

				start := 0
				if len(all) > lines {
					start = len(all) - lines
				}
				selected := all[start:]
				content := strings.Join(selected, "\n")
				return map[string]interface{}{
					"path":           logPath,
					"total_lines":    len(all),
					"selected_lines": len(selected),
					"content":        truncate(content, cfg.ToolOutputMaxBytes),
				}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "web_search",
			Description: "Search the web using Ollama web search",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "query": {"type": "string"},
    "max_results": {"type": "integer", "minimum": 1, "maximum": 10}
  },
  "required": ["query"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				query, ok := args["query"].(string)
				if !ok || strings.TrimSpace(query) == "" {
					return nil, errors.New("query is required")
				}
				maxResults := 5
				if v, ok := asInt(args["max_results"]); ok {
					maxResults = v
				}
				res, err := client.WebSearch(ctx, query, maxResults)
				if err != nil {
					return nil, err
				}
				items := make([]map[string]string, 0, len(res.Results))
				for _, r := range res.Results {
					items = append(items, map[string]string{"title": r.Title, "url": r.URL, "content": truncate(r.Content, cfg.ToolOutputMaxBytes)})
				}
				return map[string]any{"results": items}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "web_fetch",
			Description: "Fetch a web page using Ollama web fetch",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "url": {"type": "string"}
  },
  "required": ["url"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				u, ok := args["url"].(string)
				if !ok || strings.TrimSpace(u) == "" {
					return nil, errors.New("url is required")
				}
				res, err := client.WebFetch(ctx, u)
				if err != nil {
					return nil, err
				}
				return map[string]any{"title": res.Title, "content": truncate(res.Content, cfg.ToolOutputMaxBytes), "links": res.Links}, nil
			},
			Source: "builtin",
		},
	}

	if cfg.Cron != nil {
		out = append(out, cronTools(cfg.Cron)...)
	}
	return out
}

func cronTools(ctrl CronController) []Tool {
	return []Tool{
		{
			Name:        "cron_add",
			Description: "Create or update a cron job that periodically sends a prompt to OllamaClaw",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"},
    "schedule": {"type": "string", "description": "Cron schedule, e.g. '0 * * * *'"},
    "prompt": {"type": "string", "description": "Prompt to run when the job triggers"},
    "transport": {"type": "string", "description": "Target transport, defaults to current session transport"},
    "session_key": {"type": "string", "description": "Target session key, defaults to current session key"}
  },
  "required": ["schedule", "prompt"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				schedule, ok := args["schedule"].(string)
				if !ok || strings.TrimSpace(schedule) == "" {
					return nil, errors.New("schedule is required")
				}
				prompt, ok := args["prompt"].(string)
				if !ok || strings.TrimSpace(prompt) == "" {
					return nil, errors.New("prompt is required")
				}
				spec := CronJobSpec{Schedule: schedule, Prompt: prompt}
				if v, ok := args["id"].(string); ok {
					spec.ID = v
				}
				if v, ok := args["transport"].(string); ok {
					spec.Transport = v
				}
				if v, ok := args["session_key"].(string); ok {
					spec.SessionKey = v
				}
				if info, ok := SessionInfoFromContext(ctx); ok {
					if strings.TrimSpace(spec.Transport) == "" {
						spec.Transport = info.Transport
					}
					if strings.TrimSpace(spec.SessionKey) == "" {
						spec.SessionKey = info.SessionKey
					}
				}
				job, err := ctrl.AddJob(ctx, spec)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{
					"id":          job.ID,
					"schedule":    job.Schedule,
					"prompt":      job.Prompt,
					"transport":   job.Transport,
					"session_key": job.SessionKey,
					"active":      job.Active,
					"next_run_at": job.NextRunAt,
				}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "cron_list",
			Description: "List configured cron jobs",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "active_only": {"type": "boolean", "default": true}
  }
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				activeOnly := true
				if v, ok := args["active_only"].(bool); ok {
					activeOnly = v
				}
				jobs, err := ctrl.ListJobs(ctx, activeOnly)
				if err != nil {
					return nil, err
				}
				items := make([]map[string]interface{}, 0, len(jobs))
				for _, j := range jobs {
					items = append(items, map[string]interface{}{
						"id":          j.ID,
						"schedule":    j.Schedule,
						"prompt":      j.Prompt,
						"transport":   j.Transport,
						"session_key": j.SessionKey,
						"active":      j.Active,
						"last_run_at": j.LastRunAt,
						"next_run_at": j.NextRunAt,
						"last_error":  j.LastError,
					})
				}
				return map[string]interface{}{"jobs": items}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "cron_remove",
			Description: "Remove a cron job by id",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"}
  },
  "required": ["id"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				id, ok := args["id"].(string)
				if !ok || strings.TrimSpace(id) == "" {
					return nil, errors.New("id is required")
				}
				if err := ctrl.RemoveJob(ctx, id); err != nil {
					return nil, err
				}
				return map[string]interface{}{"removed": true, "id": id}, nil
			},
			Source: "builtin",
		},
	}
}

func ToolMap(tools []Tool) map[string]Tool {
	out := make(map[string]Tool, len(tools))
	for _, t := range tools {
		out[t.Name] = t
	}
	return out
}

func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	if len(s) <= max {
		return s
	}
	tail := fmt.Sprintf("\n...[truncated %d bytes]", len(s)-max)
	if len(tail) >= max {
		return s[:max]
	}
	return s[:max-len(tail)] + tail
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}

func guardTelegramBashCommand(ctx context.Context, cmd string) error {
	info, ok := SessionInfoFromContext(ctx)
	if !ok || !strings.EqualFold(strings.TrimSpace(info.Transport), "telegram") {
		return nil
	}
	reason := disallowedTelegramLifecycleReason(cmd)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("command blocked in telegram bash: %s", reason)
}

func disallowedTelegramLifecycleReason(cmd string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(cmd), " "))
	if norm == "" {
		return ""
	}
	if strings.Contains(norm, "pkill") && strings.Contains(norm, "ollamaclaw") {
		return "process-kill commands targeting ollamaclaw are not allowed in telegram sessions"
	}
	if strings.Contains(norm, "killall") && strings.Contains(norm, "ollamaclaw") {
		return "process-kill commands targeting ollamaclaw are not allowed in telegram sessions"
	}
	if (strings.Contains(norm, "rm ") || strings.Contains(norm, "unlink ")) && strings.Contains(norm, "launch.lock") {
		return "modifying launch lock files is not allowed in telegram sessions"
	}
	if strings.Contains(norm, "ollamaclaw telegram run") || strings.Contains(norm, "ollamaclaw launch") || strings.Contains(norm, "./ollamaclaw") {
		return "starting nested ollamaclaw launch/poller processes is not allowed in telegram sessions"
	}
	return ""
}

func mustSchema(s string) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(s))
}
