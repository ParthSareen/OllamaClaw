package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/parth/ollamaclaw/internal/agent"
	"github.com/parth/ollamaclaw/internal/config"
	"github.com/parth/ollamaclaw/internal/db"
	"github.com/parth/ollamaclaw/internal/ollama"
	"github.com/parth/ollamaclaw/internal/plugin"
	"github.com/parth/ollamaclaw/internal/telegram"
)

type App struct{}

func New() *App { return &App{} }

func (a *App) Run(args []string) error {
	if len(args) == 0 {
		a.printUsage()
		return nil
	}
	cmd := args[0]
	switch cmd {
	case "repl":
		return a.runRepl(args[1:])
	case "telegram":
		return a.runTelegram(args[1:])
	case "plugin":
		return a.runPlugin(args[1:])
	case "help", "--help", "-h":
		a.printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func (a *App) printUsage() {
	fmt.Print(`OllamaClaw

Usage:
  ollamaclaw repl [--model <name>]
  ollamaclaw telegram init [--token <telegram-bot-token>]
  ollamaclaw telegram run
  ollamaclaw plugin new <name>
  ollamaclaw plugin test [--path <dir>]
  ollamaclaw plugin pack [--path <dir>]
  ollamaclaw plugin install <git|url|path>
  ollamaclaw plugin list
  ollamaclaw plugin enable <plugin-id>
  ollamaclaw plugin disable <plugin-id>
  ollamaclaw plugin remove <plugin-id>
  ollamaclaw plugin update [plugin-id]
`)
}

func (a *App) runRepl(args []string) error {
	fs := flag.NewFlagSet("repl", flag.ContinueOnError)
	model := fs.String("model", "", "Model override for this REPL session")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, cleanup, err := a.bootstrap()
	if err != nil {
		return err
	}
	defer cleanup()
	if strings.TrimSpace(*model) != "" {
		sess, err := r.engine.GetOrCreateSession(context.Background(), "repl", "default")
		if err == nil {
			_ = r.engine.SetSessionModel(context.Background(), sess.ID, *model)
		}
	}
	return runREPL(context.Background(), r.engine)
}

func (a *App) runTelegram(args []string) error {
	if len(args) == 0 {
		return errors.New("telegram requires subcommand: init|run")
	}
	sub := args[0]
	switch sub {
	case "init":
		fs := flag.NewFlagSet("telegram init", flag.ContinueOnError)
		token := fs.String("token", "", "Telegram bot token")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if strings.TrimSpace(*token) == "" {
			fmt.Print("Telegram bot token: ")
			var input string
			_, _ = fmt.Scanln(&input)
			*token = strings.TrimSpace(input)
		}
		if strings.TrimSpace(*token) == "" {
			return errors.New("telegram token is required")
		}
		if err := telegram.Init(context.Background(), *token); err != nil {
			return err
		}
		cfg.Telegram.BotToken = *token
		if err := config.Save(cfg); err != nil {
			return err
		}
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return err
		}
		defer store.Close()
		fmt.Println("Telegram initialized and config saved")
		return nil
	case "run":
		r, cleanup, err := a.bootstrap()
		if err != nil {
			return err
		}
		defer cleanup()
		if strings.TrimSpace(r.cfg.Telegram.BotToken) == "" {
			return errors.New("telegram bot token is missing; run `ollamaclaw telegram init` first")
		}
		runner := telegram.Runner{
			Cfg:    r.cfg,
			Store:  r.store,
			Engine: r.engine,
		}
		return runner.Run(context.Background())
	default:
		return fmt.Errorf("unknown telegram subcommand: %s", sub)
	}
}

func (a *App) runPlugin(args []string) error {
	if len(args) == 0 {
		return errors.New("plugin requires subcommand")
	}
	sub := args[0]
	switch sub {
	case "new":
		if len(args) < 2 {
			return errors.New("usage: ollamaclaw plugin new <name>")
		}
		dir, err := plugin.Scaffold(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Created plugin scaffold at %s\n", dir)
		return nil
	case "test":
		fs := flag.NewFlagSet("plugin test", flag.ContinueOnError)
		path := fs.String("path", ".", "Plugin directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		tools, err := plugin.Test(context.Background(), *path, 30)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(tools, "", "  ")
		fmt.Println(string(b))
		return nil
	case "pack":
		fs := flag.NewFlagSet("plugin pack", flag.ContinueOnError)
		path := fs.String("path", ".", "Plugin directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		archive, checksum, err := plugin.Pack(*path)
		if err != nil {
			return err
		}
		fmt.Printf("Packed plugin: %s\nsha256: %s\n", archive, checksum)
		return nil
	case "install":
		if len(args) < 2 {
			return errors.New("usage: ollamaclaw plugin install <git|url|path>")
		}
		r, cleanup, err := a.bootstrap()
		if err != nil {
			return err
		}
		defer cleanup()
		p, t, err := r.pluginManager.Install(context.Background(), args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Installed plugin %s@%s (%d tools)\n", p.ID, p.Version, len(t))
		return nil
	case "list":
		r, cleanup, err := a.bootstrap()
		if err != nil {
			return err
		}
		defer cleanup()
		plugins, err := r.store.ListPlugins(context.Background(), false)
		if err != nil {
			return err
		}
		if len(plugins) == 0 {
			fmt.Println("No plugins installed")
			return nil
		}
		for _, p := range plugins {
			status := "disabled"
			if p.Enabled {
				status = "enabled"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", p.ID, p.Version, status, p.Source)
		}
		return nil
	case "enable", "disable", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: ollamaclaw plugin %s <plugin-id>", sub)
		}
		r, cleanup, err := a.bootstrap()
		if err != nil {
			return err
		}
		defer cleanup()
		id := args[1]
		switch sub {
		case "enable":
			err = r.pluginManager.SetEnabled(context.Background(), id, true)
		case "disable":
			err = r.pluginManager.SetEnabled(context.Background(), id, false)
		case "remove":
			err = r.pluginManager.Remove(context.Background(), id)
		}
		if err != nil {
			return err
		}
		fmt.Printf("Plugin %s %sd\n", id, sub)
		return nil
	case "update":
		var target string
		if len(args) >= 2 {
			target = args[1]
		}
		r, cleanup, err := a.bootstrap()
		if err != nil {
			return err
		}
		defer cleanup()
		if err := r.pluginManager.Update(context.Background(), target); err != nil {
			return err
		}
		if target == "" {
			fmt.Println("Updated all plugins")
		} else {
			fmt.Printf("Updated plugin %s\n", target)
		}
		return nil
	default:
		return fmt.Errorf("unknown plugin subcommand: %s", sub)
	}
}

type runtime struct {
	cfg           config.Config
	store         *db.Store
	engine        *agent.Engine
	pluginManager *plugin.Manager
}

func (a *App) bootstrap() (*runtime, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	client := ollama.NewClient(cfg.OllamaHost)
	pm := plugin.NewManager(store, cfg)
	eng := agent.New(cfg, store, client, pm)
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = store.SetSetting(ctx, "last_shutdown", time.Now().UTC().Format(time.RFC3339Nano))
		_ = store.Close()
	}
	return &runtime{cfg: cfg, store: store, engine: eng, pluginManager: pm}, cleanup, nil
}
