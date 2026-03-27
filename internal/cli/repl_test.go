package cli

import (
	"strings"
	"testing"

	"github.com/ParthSareen/OllamaClaw/internal/agent"
)

func TestParseOnOff(t *testing.T) {
	on, ok := parseOnOff("on")
	if !ok || !on {
		t.Fatalf("expected on to parse as true")
	}
	off, ok := parseOnOff("off")
	if !ok || off {
		t.Fatalf("expected off to parse as false")
	}
	if _, ok := parseOnOff("maybe"); ok {
		t.Fatalf("expected unknown input to be rejected")
	}
}

func TestFormatToolTrace(t *testing.T) {
	trace := []agent.ToolTraceEntry{
		{Name: "read_file", DurationMs: 12, ArgsJSON: `{"path":"./a.txt"}`, ResultJSON: `{"content":"ok"}`},
		{Name: "bash", DurationMs: 1, ArgsJSON: `{"command":"false"}`, Error: "exit status 1"},
	}
	out := formatToolTrace(trace)
	if !strings.Contains(out, "1. read_file (12 ms)") {
		t.Fatalf("missing first trace line: %q", out)
	}
	if !strings.Contains(out, `result={"content":"ok"}`) {
		t.Fatalf("missing result payload: %q", out)
	}
	if !strings.Contains(out, "error=exit status 1") {
		t.Fatalf("missing error payload: %q", out)
	}
}
