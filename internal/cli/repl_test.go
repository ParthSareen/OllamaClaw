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

func TestParseThinkValue(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "on", want: "on", ok: true},
		{in: "off", want: "off", ok: true},
		{in: "low", want: "low", ok: true},
		{in: "medium", want: "medium", ok: true},
		{in: "high", want: "high", ok: true},
		{in: "default", want: "default", ok: true},
		{in: "auto", want: "default", ok: true},
		{in: "nope", want: "", ok: false},
	}
	for _, tc := range tests {
		got, ok := parseThinkValue(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("parseThinkValue(%q) = (%q,%t), want (%q,%t)", tc.in, got, ok, tc.want, tc.ok)
		}
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

func TestFormatThinkingTrace(t *testing.T) {
	trace := []agent.ThinkingTraceEntry{
		{Step: 1, Thinking: "  think about plan  ", ToolCallCount: 1},
		{Step: 2, Thinking: " finalize", ToolCallCount: 0},
	}
	out := formatThinkingTrace(trace)
	if !strings.Contains(out, "thinking trace:") {
		t.Fatalf("missing header: %q", out)
	}
	if !strings.Contains(out, "step=1") || !strings.Contains(out, "tool-step (1 tool calls)") {
		t.Fatalf("missing tool-step line: %q", out)
	}
	if !strings.Contains(out, "step=2") || !strings.Contains(out, "final") {
		t.Fatalf("missing final line: %q", out)
	}
}
