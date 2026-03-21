package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/parth/ollamaclaw/internal/util"
)

func Scaffold(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name is required")
	}
	dir, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	manifest := Manifest{
		ID:         strings.ToLower(strings.ReplaceAll(name, " ", "-")),
		Name:       name,
		Version:    "0.1.0",
		APIVersion: "1.0",
		Entrypoint: Entrypoint{Command: "python3", Args: []string{"plugin.py"}},
		Protocol:   Protocol{JSONRPC: "2.0", Transport: "stdio", Framing: "ndjson"},
		Permissions: map[string]interface{}{
			"filesystem": []string{"read:*", "write:*"},
		},
	}
	b, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, manifestName), b, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.py"), []byte(samplePythonPlugin()), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(sampleReadme(name)), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

func Test(ctx context.Context, path string, timeoutSec int) ([]ToolDescriptor, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	manifest, err := parseManifest(filepath.Join(root, manifestName))
	if err != nil {
		return nil, err
	}
	return ProbeTools(ctx, root, manifest, timeoutSec)
}

func Pack(path string) (string, string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	manifest, err := parseManifest(filepath.Join(root, manifestName))
	if err != nil {
		return "", "", err
	}
	outName := fmt.Sprintf("%s-%s.tar.gz", strings.ReplaceAll(manifest.ID, "/", "_"), manifest.Version)
	outPath := filepath.Join(root, outName)
	hash, err := util.CreateTarGz(root, outPath)
	if err != nil {
		return "", "", err
	}
	return outPath, hash, nil
}

func sampleReadme(name string) string {
	return fmt.Sprintf("# %s\n\nExample OllamaClaw plugin using subprocess JSON-RPC over stdio.\n", name)
}

func samplePythonPlugin() string {
	return `#!/usr/bin/env python3
import json
import sys

TOOLS = [
    {
        "name": "echo_text",
        "description": "Echo text back from plugin",
        "parameters": {
            "type": "object",
            "properties": {
                "text": {"type": "string"}
            },
            "required": ["text"]
        },
        "timeout_seconds": 30
    }
]

def write(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    rid = req.get("id")
    method = req.get("method")
    params = req.get("params", {})

    if method == "initialize":
        write({"jsonrpc": "2.0", "id": rid, "result": {"ok": True}})
    elif method == "tools/list":
        write({"jsonrpc": "2.0", "id": rid, "result": TOOLS})
    elif method == "tools/call":
        name = params.get("name")
        args = params.get("arguments", {})
        if name == "echo_text":
            text = args.get("text", "")
            write({"jsonrpc": "2.0", "id": rid, "result": {"echo": text}})
        else:
            write({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "tool not found"}})
    elif method == "shutdown":
        write({"jsonrpc": "2.0", "id": rid, "result": {"ok": True}})
        break
    else:
        write({"jsonrpc": "2.0", "id": rid, "error": {"code": -32601, "message": "method not found"}})
`
}
