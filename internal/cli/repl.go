package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/parth/ollamaclaw/internal/agent"
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
		fmt.Println("Commands: /help /model [name] /tools /status /reset /exit")
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
		fmt.Printf("session: %s\nmodel: %s\nprompt_tokens: %d\ncompletion_tokens: %d\ncompactions: %d\n", sess.ID, sess.ModelOverride, sess.TotalPromptToken, sess.TotalEvalToken, sess.CompactionCount)
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
