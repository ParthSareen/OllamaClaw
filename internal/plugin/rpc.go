package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

type rpcClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  *bufio.Reader
	nextID  int64
	timeout time.Duration
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func newRPCClient(ctx context.Context, workDir string, m Manifest, timeoutSec int) (*rpcClient, error) {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	cmd := exec.CommandContext(ctx, m.Entrypoint.Command, m.Entrypoint.Args...)
	cmd.Dir = workDir
	cmd.Env = sanitizedEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &rpcClient{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		stderr:  bufio.NewReader(stderr),
		nextID:  1,
		timeout: time.Duration(timeoutSec) * time.Second,
	}, nil
}

func (c *rpcClient) close() {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = c.request(shutdownCtx, "shutdown", map[string]interface{}{})
	_ = c.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		_ = c.cmd.Process.Kill()
	}
}

func (c *rpcClient) request(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.nextID, 1)
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := c.stdin.Write(append(payload, '\n')); err != nil {
		return nil, err
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	for {
		select {
		case <-ctxTimeout.Done():
			return nil, fmt.Errorf("plugin rpc timeout for method %s", method)
		default:
			line, err := c.stdout.ReadBytes('\n')
			if err != nil {
				return nil, fmt.Errorf("read plugin response: %w", err)
			}
			line = []byte(strings.TrimSpace(string(line)))
			if len(line) == 0 {
				continue
			}
			var res rpcResponse
			if err := json.Unmarshal(line, &res); err != nil {
				continue
			}
			if res.ID != id {
				continue
			}
			if res.Error != nil {
				return nil, fmt.Errorf("plugin rpc error %d: %s", res.Error.Code, res.Error.Message)
			}
			return res.Result, nil
		}
	}
}

func ProbeTools(ctx context.Context, pluginDir string, manifest Manifest, timeoutSec int) ([]ToolDescriptor, error) {
	cl, err := newRPCClient(ctx, pluginDir, manifest, timeoutSec)
	if err != nil {
		return nil, err
	}
	defer cl.close()
	initParams := map[string]interface{}{
		"protocol": "1.0",
		"host":     "ollamaclaw",
	}
	if _, err := cl.request(ctx, "initialize", initParams); err != nil {
		return nil, err
	}
	raw, err := cl.request(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}
	var tools []ToolDescriptor
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("parse tools/list response: %w", err)
	}
	return tools, nil
}

func CallTool(ctx context.Context, pluginDir string, manifest Manifest, timeoutSec int, toolName string, args map[string]interface{}) (map[string]interface{}, error) {
	cl, err := newRPCClient(ctx, pluginDir, manifest, timeoutSec)
	if err != nil {
		return nil, err
	}
	defer cl.close()
	if _, err := cl.request(ctx, "initialize", map[string]interface{}{"protocol": "1.0", "host": "ollamaclaw"}); err != nil {
		return nil, err
	}
	payload := map[string]interface{}{"name": toolName, "arguments": args}
	raw, err := cl.request(ctx, "tools/call", payload)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if len(raw) == 0 {
		return map[string]interface{}{}, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode tools/call response: %w", err)
	}
	return out, nil
}

func sanitizedEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "SHELL", "LANG", "LC_ALL"}
	lookup := map[string]bool{}
	for _, k := range keep {
		lookup[k] = true
	}
	env := []string{}
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		if lookup[parts[0]] {
			env = append(env, kv)
		}
	}
	return env
}

func parseManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, err
	}
	if err := validateManifest(m); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func validateManifest(m Manifest) error {
	if strings.TrimSpace(m.ID) == "" {
		return errors.New("manifest id is required")
	}
	if strings.TrimSpace(m.Name) == "" {
		return errors.New("manifest name is required")
	}
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("manifest version is required")
	}
	if strings.TrimSpace(m.Entrypoint.Command) == "" {
		return errors.New("manifest entrypoint.command is required")
	}
	if m.Protocol.Transport == "" {
		m.Protocol.Transport = "stdio"
	}
	if m.Protocol.Transport != "stdio" {
		return errors.New("only stdio transport is supported")
	}
	if m.Protocol.Framing == "" {
		m.Protocol.Framing = "ndjson"
	}
	if m.Protocol.Framing != "ndjson" {
		return errors.New("only ndjson framing is supported")
	}
	return nil
}
