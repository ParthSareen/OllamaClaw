package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ParthSareen/OllamaClaw/internal/agent"
)

func runREPL(ctx context.Context, eng *agent.Engine) error {
	fmt.Println("OllamaClaw REPL. Type /help for commands, /exit to quit.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if err := handleREPLCommand(ctx, eng, line); err != nil {
				fmt.Printf("error: %v\n", err)
			}
			if line == "/exit" || line == "/quit" {
				return nil
			}
			continue
		}
		res, err := eng.HandleText(ctx, "repl", "default", line)
		if err != nil {
			fmt.Printf("error: %v\n", err)
			continue
		}
		if strings.TrimSpace(res.AssistantContent) == "" {
			fmt.Println("(empty response)")
		} else {
			fmt.Printf("\n%s\n", res.AssistantContent)
		}
		verbose, _ := eng.IsSessionVerbose(ctx, "repl", "default")
		if verbose && len(res.ToolTrace) > 0 {
			fmt.Printf("\n%s\n", formatToolTrace(res.ToolTrace))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func handleREPLCommand(ctx context.Context, eng *agent.Engine, cmd string) error {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return nil
	}
	switch parts[0] {
	case "/exit", "/quit":
		fmt.Println("bye")
		return nil
	case "/help":
		fmt.Println("Commands: /help /model [name] /tools /verbose [on|off] /think [on|off] /status /reset /exit")
		return nil
	case "/model":
		sess, err := eng.GetOrCreateSession(ctx, "repl", "default")
		if err != nil {
			return err
		}
		if len(parts) == 1 {
			fmt.Printf("model: %s\n", sess.ModelOverride)
			return nil
		}
		model := strings.TrimSpace(strings.Join(parts[1:], " "))
		if model == "" {
			return nil
		}
		if err := eng.SetSessionModel(ctx, sess.ID, model); err != nil {
			return err
		}
		fmt.Printf("model set to: %s\n", model)
		return nil
	case "/tools":
		tools, err := eng.ListTools(ctx)
		if err != nil {
			return err
		}
		fmt.Println("available tools:")
		for _, t := range tools {
			if t.Source == "plugin" {
				fmt.Printf("- %s (plugin:%s)\n", t.Name, t.PluginID)
			} else {
				fmt.Printf("- %s\n", t.Name)
			}
		}
		return nil
	case "/status":
		sess, err := eng.GetOrCreateSession(ctx, "repl", "default")
		if err != nil {
			return err
		}
		verbose, _ := eng.IsSessionVerbose(ctx, "repl", "default")
		think, _ := eng.IsSessionThink(ctx, "repl", "default")
		fmt.Printf("session: %s\nmodel: %s\nverbose: %t\nthink: %t\nprompt_tokens: %d\ncompletion_tokens: %d\ncompactions: %d\n", sess.ID, sess.ModelOverride, verbose, think, sess.TotalPromptToken, sess.TotalEvalToken, sess.CompactionCount)
		return nil
	case "/verbose":
		const transport = "repl"
		const sessionKey = "default"
		if len(parts) == 1 {
			enabled, err := eng.IsSessionVerbose(ctx, transport, sessionKey)
			if err != nil {
				return err
			}
			fmt.Printf("verbose: %t\n", enabled)
			return nil
		}
		enabled, ok := parseOnOff(parts[1])
		if !ok {
			fmt.Println("usage: /verbose [on|off]")
			return nil
		}
		if err := eng.SetSessionVerbose(ctx, transport, sessionKey, enabled); err != nil {
			return err
		}
		fmt.Printf("verbose: %t\n", enabled)
		return nil
	case "/think":
		const transport = "repl"
		const sessionKey = "default"
		if len(parts) == 1 {
			enabled, err := eng.IsSessionThink(ctx, transport, sessionKey)
			if err != nil {
				return err
			}
			fmt.Printf("think: %t\n", enabled)
			return nil
		}
		enabled, ok := parseOnOff(parts[1])
		if !ok {
			fmt.Println("usage: /think [on|off]")
			return nil
		}
		if err := eng.SetSessionThink(ctx, transport, sessionKey, enabled); err != nil {
			return err
		}
		fmt.Printf("think: %t\n", enabled)
		return nil
	case "/reset":
		sess, err := eng.ResetSession(ctx, "repl", "default")
		if err != nil {
			return err
		}
		fmt.Printf("session reset: %s\n", sess.ID)
		return nil
	default:
		fmt.Printf("unknown command: %s\n", parts[0])
		return nil
	}
}

func parseOnOff(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "1", "true", "yes":
		return true, true
	case "off", "0", "false", "no":
		return false, true
	default:
		return false, false
	}
}

func formatToolTrace(trace []agent.ToolTraceEntry) string {
	if len(trace) == 0 {
		return "tool calls: (none)"
	}
	lines := []string{"tool calls:"}
	for i, entry := range trace {
		line := fmt.Sprintf("%d. %s (%d ms)", i+1, entry.Name, entry.DurationMs)
		if strings.TrimSpace(entry.ArgsJSON) != "" {
			line += " args=" + entry.ArgsJSON
		}
		if strings.TrimSpace(entry.Error) != "" {
			line += " error=" + entry.Error
		} else if strings.TrimSpace(entry.ResultJSON) != "" {
			line += " result=" + entry.ResultJSON
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
