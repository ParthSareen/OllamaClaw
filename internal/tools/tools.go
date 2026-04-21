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
	"regexp"
	"strings"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/ollama"
	"github.com/ParthSareen/OllamaClaw/internal/util"
)

type Executor func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error)

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Execute     Executor
	Source      string
	TimeoutSec  int
}

type ReminderSpec struct {
	ID           string
	Prompt       string
	Transport    string
	SessionKey   string
	Safe         bool
	AutoPrefetch *bool

	Mode         string
	Date         string
	Time         string
	IntervalUnit string
	Interval     int
	Minute       *int
	Days         []string
	DayOfMonth   int
}

type ReminderInfo struct {
	ID               string                 `json:"id"`
	Mode             string                 `json:"mode"`
	CompiledSchedule string                 `json:"compiled_schedule"`
	Prompt           string                 `json:"prompt"`
	Transport        string                 `json:"transport"`
	SessionKey       string                 `json:"session_key"`
	Active           bool                   `json:"active"`
	Safe             bool                   `json:"safe"`
	AutoPrefetch     bool                   `json:"auto_prefetch"`
	Spec             map[string]interface{} `json:"spec,omitempty"`
	OnceFireAt       string                 `json:"once_fire_at,omitempty"`
	LastRunAt        string                 `json:"last_run_at,omitempty"`
	NextRunAt        string                 `json:"next_run_at,omitempty"`
	LastError        string                 `json:"last_error,omitempty"`
}

type ReminderController interface {
	AddReminder(ctx context.Context, spec ReminderSpec) (ReminderInfo, error)
	ListReminders(ctx context.Context, activeOnly bool) ([]ReminderInfo, error)
	RemoveReminder(ctx context.Context, id string) error
}

type BuiltinsConfig struct {
	ToolOutputMaxBytes int
	BashTimeoutSec     int
	LogPath            string
	Reminders          ReminderController
}

const (
	defaultBashTimeoutSec      = 120
	maxBashTimeoutSec          = 120
	maxPromptOverlayChars      = 4000
	defaultPromptHistoryLimit  = 10
	maxPromptHistoryLimit      = 50
	promptOverlayPreviewMaxRun = 240
)

type sessionContextKey struct{}
type prefetchContextKey struct{}

type SessionInfo struct {
	Transport  string
	SessionKey string
}

type PrefetchedBashResult struct {
	Command    string `json:"command"`
	RunID      string `json:"run_id"`
	RunStarted string `json:"run_started_at"`
	FetchedAt  string `json:"fetched_at"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

type BashApprovalRequest struct {
	Command     string
	Normalized  string
	Reason      string
	AllowAlways bool
}

type BashApprover interface {
	ApproveBashCommand(ctx context.Context, req BashApprovalRequest) error
}

type promptOverlayHistoryEntry struct {
	Revision  string `json:"revision"`
	CreatedAt string `json:"created_at"`
	Operation string `json:"operation"`
	Note      string `json:"note,omitempty"`
	Overlay   string `json:"overlay"`
}

type telegramBashPolicy int

const (
	telegramBashPolicyAllow telegramBashPolicy = iota
	telegramBashPolicyRequireApproval
	telegramBashPolicyDeny
)

type telegramDestructivePattern struct {
	RX     *regexp.Regexp
	Reason string
}

var (
	telegramDenyPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bsudo\b`),
		regexp.MustCompile(`\bdoas\b`),
		regexp.MustCompile(`\brm\s+-rf\s+/(?:\s|$)`),
	}
	telegramExplicitApprovalPatterns = []telegramDestructivePattern{
		{
			RX:     regexp.MustCompile(`^curl(?:\s|$)`),
			Reason: "network/data command requires explicit approval",
		},
		{
			RX:     regexp.MustCompile(`^wget(?:\s|$)`),
			Reason: "network/data command requires explicit approval",
		},
		{
			RX:     regexp.MustCompile(`^http(?:\s|$)`), // httpie
			Reason: "network/data command requires explicit approval",
		},
		{
			RX:     regexp.MustCompile(`^scp(?:\s|$)`),
			Reason: "network/data command requires explicit approval",
		},
		{
			RX:     regexp.MustCompile(`^ssh(?:\s|$)`),
			Reason: "network/data command requires explicit approval",
		},
	}
	telegramPotentiallyDestructivePatterns = []telegramDestructivePattern{
		{
			RX:     regexp.MustCompile(`\b(?:rm|rmdir|unlink|shred|wipefs|mkfs(?:\.[-\w]+)?|fdisk|parted|dd)\b`),
			Reason: "contains potentially destructive file/system commands",
		},
		{
			RX:     regexp.MustCompile(`\b(?:mv|cp|install|rsync|truncate|touch|mkdir|chmod|chown|chgrp|chflags|xattr)\b`),
			Reason: "contains filesystem mutation commands",
		},
		{
			RX:     regexp.MustCompile(`\b(?:sed|perl)\b[^\n]*\s-i(?:\s|$)`),
			Reason: "contains in-place file edits",
		},
		{
			RX:     regexp.MustCompile(`\bgit\s+(?:add|commit|merge|rebase|cherry-pick|am|apply|reset|clean|checkout|switch|restore|stash|tag|branch|push|pull)\b`),
			Reason: "contains mutating git subcommands",
		},
		{
			RX:     regexp.MustCompile(`\b(?:kill|pkill|killall|launchctl|systemctl|service|shutdown|reboot|halt|poweroff)\b`),
			Reason: "contains process/service control commands",
		},
		{
			RX:     regexp.MustCompile(`\b(?:apt|apt-get|yum|dnf|pacman|brew|pip|pip3|npm|pnpm|yarn|cargo|go)\b\s+(?:install|uninstall|remove|upgrade|update|dist-upgrade|full-upgrade|autoremove|clean|tap|untap|get)\b`),
			Reason: "contains package manager mutation commands",
		},
		{
			RX:     regexp.MustCompile(`\b(?:curl|wget)\b[^\n]*\s(?:-x|--request)\s*(?:post|put|patch|delete)\b`),
			Reason: "contains network write operations",
		},
		{
			RX:     regexp.MustCompile(`\bhttp\s+(?:post|put|patch|delete)\b`),
			Reason: "contains network write operations",
		},
		{
			RX:     regexp.MustCompile(`\b(?:bash|sh|zsh)\s+-c\b`),
			Reason: "contains nested shell execution",
		},
	}
)

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

func WithPrefetchedBashResults(ctx context.Context, results []PrefetchedBashResult) context.Context {
	if len(results) == 0 {
		return ctx
	}
	copied := make([]PrefetchedBashResult, len(results))
	copy(copied, results)
	return context.WithValue(ctx, prefetchContextKey{}, copied)
}

func PrefetchedBashResultsFromContext(ctx context.Context) ([]PrefetchedBashResult, bool) {
	v := ctx.Value(prefetchContextKey{})
	if v == nil {
		return nil, false
	}
	results, ok := v.([]PrefetchedBashResult)
	if !ok || len(results) == 0 {
		return nil, false
	}
	copied := make([]PrefetchedBashResult, len(results))
	copy(copied, results)
	return copied, true
}

type bashApproverContextKey struct{}

func WithBashApprover(ctx context.Context, approver BashApprover) context.Context {
	return context.WithValue(ctx, bashApproverContextKey{}, approver)
}

func BashApproverFromContext(ctx context.Context) (BashApprover, bool) {
	v := ctx.Value(bashApproverContextKey{})
	if v == nil {
		return nil, false
	}
	approver, ok := v.(BashApprover)
	return approver, ok
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
    "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": 120}
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
				timeout := effectiveBashTimeoutSec(cfg.BashTimeoutSec, args)
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
					res["stderr"] = truncate(res["stderr"].(string)+fmt.Sprintf("\ncommand timed out after %ds", timeout), cfg.ToolOutputMaxBytes)
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
			Name:        "system_prompt_get",
			Description: "Read managed system prompt paths/content and optional revision history",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "include_base": {"type": "boolean", "description": "Include system_prompt.txt content in response", "default": false},
    "history_limit": {"type": "integer", "minimum": 0, "maximum": 50, "description": "Include last N overlay history revisions (0 disables history)", "default": 0}
  }
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				_ = ctx
				includeBase := false
				if v, ok := args["include_base"].(bool); ok {
					includeBase = v
				}
				historyLimit := 0
				if v, ok := asInt(args["history_limit"]); ok {
					if v < 0 {
						v = 0
					}
					if v > maxPromptHistoryLimit {
						v = maxPromptHistoryLimit
					}
					historyLimit = v
				}
				basePath, err := config.SystemPromptPath()
				if err != nil {
					return nil, err
				}
				overlayPath, err := config.SystemPromptOverlayPath()
				if err != nil {
					return nil, err
				}
				historyPath, err := config.SystemPromptOverlayHistoryPath()
				if err != nil {
					return nil, err
				}
				overlay, err := readPromptOverlay()
				if err != nil {
					return nil, err
				}
				out := map[string]interface{}{
					"base_path":            basePath,
					"overlay_path":         overlayPath,
					"history_path":         historyPath,
					"overlay":              overlay,
					"overlay_chars":        len([]rune(overlay)),
					"max_overlay_chars":    maxPromptOverlayChars,
					"timezone":             util.PacificTimezoneName,
					"managed_overlay_mode": true,
				}
				if includeBase {
					b, err := os.ReadFile(basePath)
					if err == nil {
						out["base_prompt"] = string(b)
					} else if errors.Is(err, os.ErrNotExist) {
						out["base_prompt"] = ""
						out["base_prompt_missing"] = true
					} else {
						return nil, err
					}
				}
				if historyLimit > 0 {
					history, err := readPromptOverlayHistory()
					if err != nil {
						return nil, err
					}
					out["history"] = historySummaries(history, historyLimit)
				}
				return out, nil
			},
			Source: "builtin",
		},
		{
			Name:        "system_prompt_update",
			Description: "Safely update only the managed system prompt overlay (set, append, clear) with revision history",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "operation": {"type": "string", "enum": ["set", "append", "clear"], "default": "set"},
    "content": {"type": "string", "description": "Overlay content for set/append operations"},
    "note": {"type": "string", "description": "Optional short note for revision history"}
  }
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				_ = ctx
				op := "set"
				if v, ok := args["operation"].(string); ok && strings.TrimSpace(v) != "" {
					op = strings.ToLower(strings.TrimSpace(v))
				}
				content := ""
				if v, ok := args["content"].(string); ok {
					content = v
				}
				note := ""
				if v, ok := args["note"].(string); ok {
					note = strings.TrimSpace(v)
				}
				current, err := readPromptOverlay()
				if err != nil {
					return nil, err
				}
				var next string
				switch op {
				case "set":
					if strings.TrimSpace(content) == "" {
						return nil, errors.New("content is required for set")
					}
					next = strings.TrimSpace(content)
				case "append":
					if strings.TrimSpace(content) == "" {
						return nil, errors.New("content is required for append")
					}
					if strings.TrimSpace(current) == "" {
						next = strings.TrimSpace(content)
					} else {
						next = strings.TrimSpace(current) + "\n\n" + strings.TrimSpace(content)
					}
				case "clear":
					next = ""
				default:
					return nil, fmt.Errorf("unsupported operation %q", op)
				}
				next, truncated := clampPromptOverlay(next)
				overlayPath, err := writePromptOverlay(next)
				if err != nil {
					return nil, err
				}
				entry, err := appendPromptOverlayHistory(op, note, next)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{
					"overlay_path":      overlayPath,
					"operation":         op,
					"revision":          entry.Revision,
					"truncated":         truncated,
					"overlay_chars":     len([]rune(next)),
					"max_overlay_chars": maxPromptOverlayChars,
					"overlay_preview":   previewPromptOverlay(next, promptOverlayPreviewMaxRun),
				}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "system_prompt_history",
			Description: "List managed system prompt overlay revisions",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "limit": {"type": "integer", "minimum": 1, "maximum": 50, "default": 10}
  }
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				_ = ctx
				limit := defaultPromptHistoryLimit
				if v, ok := asInt(args["limit"]); ok && v > 0 {
					if v > maxPromptHistoryLimit {
						v = maxPromptHistoryLimit
					}
					limit = v
				}
				history, err := readPromptOverlayHistory()
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{
					"history": historySummaries(history, limit),
				}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "system_prompt_rollback",
			Description: "Rollback managed system prompt overlay to a previous revision",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "revision": {"type": "string", "description": "Target revision from system_prompt_history"},
    "note": {"type": "string", "description": "Optional rollback reason"}
  },
  "required": ["revision"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				_ = ctx
				revision, ok := args["revision"].(string)
				if !ok || strings.TrimSpace(revision) == "" {
					return nil, errors.New("revision is required")
				}
				note := ""
				if v, ok := args["note"].(string); ok {
					note = strings.TrimSpace(v)
				}
				history, err := readPromptOverlayHistory()
				if err != nil {
					return nil, err
				}
				target, found := findPromptOverlayRevision(history, strings.TrimSpace(revision))
				if !found {
					return nil, fmt.Errorf("revision %q not found", revision)
				}
				overlay, truncated := clampPromptOverlay(target.Overlay)
				overlayPath, err := writePromptOverlay(overlay)
				if err != nil {
					return nil, err
				}
				op := "rollback"
				if note == "" {
					note = "rollback to " + target.Revision
				}
				entry, err := appendPromptOverlayHistory(op, note, overlay)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{
					"overlay_path":       overlayPath,
					"rolled_back_to":     target.Revision,
					"new_revision":       entry.Revision,
					"truncated":          truncated,
					"overlay_chars":      len([]rune(overlay)),
					"overlay_preview":    previewPromptOverlay(overlay, promptOverlayPreviewMaxRun),
					"max_overlay_chars":  maxPromptOverlayChars,
					"history_entry_note": note,
				}, nil
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

	if cfg.Reminders != nil {
		out = append(out, reminderTools(cfg.Reminders)...)
	}
	return out
}

func reminderTools(ctrl ReminderController) []Tool {
	return []Tool{
		{
			Name:        "reminder_add",
			Description: "Create or update a reminder in America/Los_Angeles (PST/PDT). The reminder is deterministically compiled to a cron schedule for runtime triggering.",
			Schema: mustSchema(`{
  "type": "object",
  "properties": {
    "id": {"type": "string"},
    "prompt": {"type": "string", "description": "Prompt to run when the reminder triggers"},
    "mode": {"type": "string", "enum": ["once", "interval", "weekdays", "monthly"]},
    "date": {"type": "string", "description": "For once mode: YYYY-MM-DD in America/Los_Angeles"},
    "time": {"type": "string", "description": "For once/day/weekdays/monthly modes: HH:MM in America/Los_Angeles"},
    "interval_unit": {"type": "string", "enum": ["minute", "hour", "day"], "description": "For interval mode"},
    "interval": {"type": "integer", "description": "For interval mode: >= 1"},
    "minute": {"type": "integer", "description": "For interval hour mode: minute of the hour (default 0)"},
    "days": {"type": "array", "items": {"type": "string"}, "description": "For weekdays mode: subset of [mon,tue,wed,thu,fri,sat,sun]"},
    "day_of_month": {"type": "integer", "description": "For monthly mode: 1..31"},
    "safe": {"type": "boolean", "description": "When true, Telegram bash approvals are auto-approved for this reminder"},
    "auto_prefetch": {"type": "boolean", "description": "When true, model-chosen stable bash commands can be prefetched automatically on future runs"},
    "transport": {"type": "string", "description": "Target transport, defaults to current session transport"},
    "session_key": {"type": "string", "description": "Target session key, defaults to current session key"}
  },
  "required": ["mode", "prompt"]
}`),
			Execute: func(ctx context.Context, args map[string]interface{}) (map[string]interface{}, error) {
				mode, ok := args["mode"].(string)
				if !ok || strings.TrimSpace(mode) == "" {
					return nil, errors.New("mode is required")
				}
				prompt, ok := args["prompt"].(string)
				if !ok || strings.TrimSpace(prompt) == "" {
					return nil, errors.New("prompt is required")
				}
				spec := ReminderSpec{Mode: mode, Prompt: prompt}
				if v, ok := args["id"].(string); ok {
					spec.ID = v
				}
				if v, ok := args["transport"].(string); ok {
					spec.Transport = v
				}
				if v, ok := args["session_key"].(string); ok {
					spec.SessionKey = v
				}
				if v, ok := args["safe"].(bool); ok {
					spec.Safe = v
				}
				if v, ok := args["auto_prefetch"].(bool); ok {
					spec.AutoPrefetch = &v
				}
				if v, ok := args["date"].(string); ok {
					spec.Date = v
				}
				if v, ok := args["time"].(string); ok {
					spec.Time = v
				}
				if v, ok := args["interval_unit"].(string); ok {
					spec.IntervalUnit = v
				}
				if v, ok := asInt(args["interval"]); ok {
					spec.Interval = v
				}
				if v, ok := asInt(args["minute"]); ok {
					spec.Minute = &v
				}
				if rawDays, ok := args["days"].([]interface{}); ok {
					days := make([]string, 0, len(rawDays))
					for _, raw := range rawDays {
						day, ok := raw.(string)
						if !ok {
							continue
						}
						days = append(days, day)
					}
					spec.Days = days
				}
				if v, ok := asInt(args["day_of_month"]); ok {
					spec.DayOfMonth = v
				}
				if info, ok := SessionInfoFromContext(ctx); ok {
					if strings.TrimSpace(spec.Transport) == "" {
						spec.Transport = info.Transport
					}
					if strings.TrimSpace(spec.SessionKey) == "" {
						spec.SessionKey = info.SessionKey
					}
				}
				reminder, err := ctrl.AddReminder(ctx, spec)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{
					"id":                reminder.ID,
					"mode":              reminder.Mode,
					"compiled_schedule": reminder.CompiledSchedule,
					"prompt":            reminder.Prompt,
					"transport":         reminder.Transport,
					"session_key":       reminder.SessionKey,
					"active":            reminder.Active,
					"safe":              reminder.Safe,
					"auto_prefetch":     reminder.AutoPrefetch,
					"spec":              reminder.Spec,
					"once_fire_at":      reminder.OnceFireAt,
					"next_run_at":       reminder.NextRunAt,
					"timezone":          util.PacificTimezoneName,
				}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "reminder_list",
			Description: "List configured reminders (timestamps returned in America/Los_Angeles, PST/PDT)",
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
				reminders, err := ctrl.ListReminders(ctx, activeOnly)
				if err != nil {
					return nil, err
				}
				items := make([]map[string]interface{}, 0, len(reminders))
				for _, j := range reminders {
					items = append(items, map[string]interface{}{
						"id":                j.ID,
						"mode":              j.Mode,
						"compiled_schedule": j.CompiledSchedule,
						"prompt":            j.Prompt,
						"transport":         j.Transport,
						"session_key":       j.SessionKey,
						"active":            j.Active,
						"safe":              j.Safe,
						"auto_prefetch":     j.AutoPrefetch,
						"spec":              j.Spec,
						"once_fire_at":      j.OnceFireAt,
						"last_run_at":       j.LastRunAt,
						"next_run_at":       j.NextRunAt,
						"last_error":        j.LastError,
						"timezone":          util.PacificTimezoneName,
					})
				}
				return map[string]interface{}{"reminders": items}, nil
			},
			Source: "builtin",
		},
		{
			Name:        "reminder_remove",
			Description: "Remove a reminder by id",
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
				if err := ctrl.RemoveReminder(ctx, id); err != nil {
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

func effectiveBashTimeoutSec(configured int, args map[string]interface{}) int {
	timeout := configured
	if v, ok := asInt(args["timeout_seconds"]); ok && v > 0 {
		timeout = v
	}
	return clampBashTimeoutSec(timeout)
}

func clampBashTimeoutSec(timeout int) int {
	if timeout <= 0 {
		return defaultBashTimeoutSec
	}
	if timeout > maxBashTimeoutSec {
		return maxBashTimeoutSec
	}
	return timeout
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

func readPromptOverlay() (string, error) {
	path, err := config.SystemPromptOverlayPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writePromptOverlay(content string) (string, error) {
	path, err := config.SystemPromptOverlayPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func appendPromptOverlayHistory(operation, note, overlay string) (promptOverlayHistoryEntry, error) {
	path, err := config.SystemPromptOverlayHistoryPath()
	if err != nil {
		return promptOverlayHistoryEntry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return promptOverlayHistoryEntry{}, err
	}
	entry := promptOverlayHistoryEntry{
		Revision:  fmt.Sprintf("%d", time.Now().UTC().UnixNano()),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Operation: strings.TrimSpace(operation),
		Note:      strings.TrimSpace(note),
		Overlay:   strings.TrimSpace(overlay),
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return promptOverlayHistoryEntry{}, err
	}
	defer f.Close()
	b, err := json.Marshal(entry)
	if err != nil {
		return promptOverlayHistoryEntry{}, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return promptOverlayHistoryEntry{}, err
	}
	return entry, nil
}

func readPromptOverlayHistory() ([]promptOverlayHistoryEntry, error) {
	path, err := config.SystemPromptOverlayHistoryPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []promptOverlayHistoryEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries := make([]promptOverlayHistoryEntry, 0, 64)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry promptOverlayHistoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if strings.TrimSpace(entry.Revision) == "" {
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

func historySummaries(entries []promptOverlayHistoryEntry, limit int) []map[string]interface{} {
	if limit <= 0 {
		return []map[string]interface{}{}
	}
	if limit > len(entries) {
		limit = len(entries)
	}
	out := make([]map[string]interface{}, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := entries[i]
		out = append(out, map[string]interface{}{
			"revision":      e.Revision,
			"created_at":    e.CreatedAt,
			"operation":     e.Operation,
			"note":          e.Note,
			"overlay_chars": len([]rune(e.Overlay)),
			"preview":       previewPromptOverlay(e.Overlay, promptOverlayPreviewMaxRun),
		})
	}
	return out
}

func findPromptOverlayRevision(entries []promptOverlayHistoryEntry, revision string) (promptOverlayHistoryEntry, bool) {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return promptOverlayHistoryEntry{}, false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Revision == revision {
			return entries[i], true
		}
	}
	return promptOverlayHistoryEntry{}, false
}

func clampPromptOverlay(content string) (string, bool) {
	text := strings.TrimSpace(content)
	if text == "" {
		return "", false
	}
	runes := []rune(text)
	if len(runes) <= maxPromptOverlayChars {
		return text, false
	}
	return strings.TrimSpace(string(runes[:maxPromptOverlayChars])), true
}

func previewPromptOverlay(content string, max int) string {
	text := strings.TrimSpace(content)
	if max <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
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
	normalized := normalizeTelegramBashCommand(cmd)
	decision, reason := classifyTelegramBashCommand(normalized)
	switch decision {
	case telegramBashPolicyDeny:
		return fmt.Errorf("command blocked in telegram bash: %s", reason)
	case telegramBashPolicyAllow:
		return nil
	default:
		approver, ok := BashApproverFromContext(ctx)
		if !ok {
			return fmt.Errorf("command requires approval in telegram bash: %s", reason)
		}
		if err := approver.ApproveBashCommand(ctx, BashApprovalRequest{
			Command:     cmd,
			Normalized:  normalized,
			Reason:      reason,
			AllowAlways: canAlwaysAllowTelegramCommand(normalized),
		}); err != nil {
			return err
		}
		return nil
	}
}

func classifyTelegramBashCommand(normalized string) (telegramBashPolicy, string) {
	if normalized == "" {
		return telegramBashPolicyRequireApproval, "empty command"
	}
	if reason := disallowedTelegramLifecycleReason(normalized); reason != "" {
		return telegramBashPolicyDeny, reason
	}
	for _, rx := range telegramDenyPatterns {
		if rx.MatchString(normalized) {
			return telegramBashPolicyDeny, "matches a denied command pattern"
		}
	}
	if reason := explicitApprovalTelegramReason(normalized); reason != "" {
		return telegramBashPolicyRequireApproval, reason
	}
	if reason := potentiallyDestructiveTelegramReason(normalized); reason != "" {
		return telegramBashPolicyRequireApproval, reason
	}
	if containsPotentialWriteRedirection(normalized) {
		return telegramBashPolicyRequireApproval, "contains file-writing output redirection"
	}
	return telegramBashPolicyAllow, ""
}

func normalizeTelegramBashCommand(cmd string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(cmd)), " "))
}

func canAlwaysAllowTelegramCommand(_ string) bool {
	return true
}

func potentiallyDestructiveTelegramReason(normalized string) string {
	for _, p := range telegramPotentiallyDestructivePatterns {
		if p.RX.MatchString(normalized) {
			return p.Reason
		}
	}
	return ""
}

func explicitApprovalTelegramReason(normalized string) string {
	for _, p := range telegramExplicitApprovalPatterns {
		if p.RX.MatchString(normalized) {
			return p.Reason
		}
	}
	return ""
}

func containsPotentialWriteRedirection(normalized string) bool {
	if normalized == "" {
		return false
	}
	fields := strings.Fields(normalized)
	for i, field := range fields {
		_, target, ok := parseWriteRedirectToken(field)
		if !ok {
			continue
		}
		if target == "" && i+1 < len(fields) {
			target = fields[i+1]
		}
		target = strings.TrimSpace(target)
		if target == "" {
			return true
		}
		if target == "/dev/null" {
			continue
		}
		return true
	}
	return false
}

func parseWriteRedirectToken(token string) (op, target string, ok bool) {
	for _, prefix := range []string{"1>>", "2>>", ">>", "1>", "2>", ">"} {
		if token == prefix {
			return prefix, "", true
		}
		if strings.HasPrefix(token, prefix) {
			return prefix, strings.TrimSpace(token[len(prefix):]), true
		}
	}
	return "", "", false
}

func disallowedTelegramLifecycleReason(normalized string) string {
	norm := normalized
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
