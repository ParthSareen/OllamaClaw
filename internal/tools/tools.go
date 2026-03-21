package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/parth/ollamaclaw/internal/ollama"
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

type BuiltinsConfig struct {
	ToolOutputMaxBytes int
	BashTimeoutSec     int
}

func BuiltinTools(cfg BuiltinsConfig, client *ollama.Client) []Tool {
	return []Tool{
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
						return map[string]interface{}{"exit_code": -1, "stdout": "", "stderr": err.Error()}, nil
					}
				}
				out := map[string]interface{}{
					"exit_code": exitCode,
					"stdout":    truncate(string(stdout), cfg.ToolOutputMaxBytes),
					"stderr":    truncate(stderr, cfg.ToolOutputMaxBytes),
				}
				if ctxTimeout.Err() == context.DeadlineExceeded {
					out["stderr"] = truncate(out["stderr"].(string)+"\ncommand timed out", cfg.ToolOutputMaxBytes)
					out["exit_code"] = -1
				}
				return out, nil
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
				return map[string]interface{}{"path": p, "content": truncate(string(b), cfg.ToolOutputMaxBytes)}, nil
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
				return map[string]interface{}{"path": p, "bytes_written": len(content)}, nil
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
				return map[string]interface{}{"results": items}, nil
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
				return map[string]interface{}{
					"title":   res.Title,
					"content": truncate(res.Content, cfg.ToolOutputMaxBytes),
					"links":   res.Links,
				}, nil
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

func mustSchema(s string) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(s))
}
