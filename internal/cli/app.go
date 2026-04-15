package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/agent"
	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/cronjobs"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/ollama"
	"github.com/ParthSareen/OllamaClaw/internal/plugin"
	"github.com/ParthSareen/OllamaClaw/internal/telegram"
)

type App struct{}

var (
	BuildVersion = "0.1.6"
	BuildCommit  = "unknown"
	BuildDate    = "unknown"
)

func New() *App { return &App{} }

func (a *App) Run(args []string) error {
	if len(args) == 0 {
		return a.runLaunch(nil)
	}
	cmd := args[0]
	switch cmd {
	case "repl":
		return a.runRepl(args[1:])
	case "launch":
		return a.runLaunch(args[1:])
	case "configure":
		return a.runConfigure(args[1:])
	case "telegram":
		return a.runTelegram(args[1:])
	case "plugin":
		return a.runPlugin(args[1:])
	case "version", "--version":
		fmt.Println(runtimeBuildLabel())
		return nil
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
  ollamaclaw launch
  ollamaclaw configure
  ollamaclaw telegram init [--token <telegram-bot-token>] [--owner-id <id>] [--owner-chat-id <id>] [--owner-user-id <id>]
  ollamaclaw telegram run (legacy alias)
  ollamaclaw plugin new <name>
  ollamaclaw plugin test [--path <dir>]
  ollamaclaw plugin pack [--path <dir>]
  ollamaclaw plugin install <git|url|path>
  ollamaclaw plugin list
  ollamaclaw plugin enable <plugin-id>
  ollamaclaw plugin disable <plugin-id>
  ollamaclaw plugin remove <plugin-id>
  ollamaclaw plugin update [plugin-id]
  ollamaclaw version
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
	r.cron.SetOutputSink(func(ctx context.Context, transport, sessionKey, content string) error {
		fmt.Printf("\n[cron %s/%s]\n%s\n", transport, sessionKey, content)
		return nil
	})
	if err := r.cron.Start(context.Background()); err != nil {
		return err
	}
	defer r.cron.Stop()
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
		ownerID := fs.Int64("owner-id", 0, "Allowlisted Telegram owner id for both chat and user (server-side)")
		ownerChatID := fs.Int64("owner-chat-id", 0, "Allowlisted Telegram chat id (server-side)")
		ownerUserID := fs.Int64("owner-user-id", 0, "Allowlisted Telegram user id (server-side)")
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
		resolvedChatID, resolvedUserID := normalizeOwnerIDs(*ownerID, *ownerChatID, *ownerUserID)
		if resolvedChatID != 0 {
			cfg.Telegram.OwnerChatID = resolvedChatID
		}
		if resolvedUserID != 0 {
			cfg.Telegram.OwnerUserID = resolvedUserID
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return err
		}
		defer store.Close()
		fmt.Println("Telegram initialized and config saved")
		if cfg.Telegram.OwnerChatID == 0 || cfg.Telegram.OwnerUserID == 0 {
			fmt.Println("warning: owner allowlist is not set. Run init with --owner-id (or --owner-chat-id and --owner-user-id) before launch.")
		}
		return nil
	case "run":
		return a.runLaunch(args[1:])
	default:
		return fmt.Errorf("unknown telegram subcommand: %s", sub)
	}
}

func (a *App) runLaunch(args []string) error {
	if len(args) != 0 {
		return errors.New("launch takes no arguments")
	}
	buildLabel := runtimeBuildLabel()
	fmt.Printf("ollamaclaw %s\n", buildLabel)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !launchConfigReady(cfg) {
		if !isInteractiveTerminal() {
			return launchConfigError(cfg)
		}
		fmt.Println("OllamaClaw needs configuration before launch.")
		fmt.Println("Opening setup UI...")
		if err := a.runConfigure(nil); err != nil {
			return err
		}
		cfg, err = config.Load()
		if err != nil {
			return err
		}
		if !launchConfigReady(cfg) {
			return launchConfigError(cfg)
		}
	}
	if conflicts, err := findOtherLaunchProcesses(os.Getpid()); err == nil && len(conflicts) > 0 {
		return fmt.Errorf("another local OllamaClaw process is already running (%s); stop it and retry", strings.Join(conflicts, "; "))
	}
	lockPath, releaseLock, err := acquireLaunchLock()
	if err != nil {
		return err
	}
	defer releaseLock()
	fmt.Printf("launch lock acquired: %s\n", lockPath)

	r, cleanup, err := a.bootstrap()
	if err != nil {
		return err
	}
	defer cleanup()
	if !launchConfigReady(r.cfg) {
		return launchConfigError(r.cfg)
	}
	runner := telegram.Runner{
		Cfg:        r.cfg,
		Store:      r.store,
		Engine:     r.engine,
		Scheduler:  r.cron,
		AppVersion: buildLabel,
	}
	for {
		err := runner.Run(context.Background())
		if errors.Is(err, telegram.ErrRestartRequested) {
			fmt.Println("launch restart requested from Telegram; relaunching...")
			continue
		}
		return err
	}
}

func runtimeBuildLabel() string {
	version := strings.TrimSpace(BuildVersion)
	if version == "" {
		version = "0.1.6"
	}
	parts := []string{version}
	commit := strings.TrimSpace(BuildCommit)
	if commit != "" && !strings.EqualFold(commit, "unknown") {
		if len(commit) > 12 {
			commit = commit[:12]
		}
		parts = append(parts, "commit="+commit)
	}
	buildDate := strings.TrimSpace(BuildDate)
	if buildDate != "" && !strings.EqualFold(buildDate, "unknown") {
		parts = append(parts, "built="+buildDate)
	}
	return strings.Join(parts, " ")
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

func launchConfigReady(cfg config.Config) bool {
	return strings.TrimSpace(cfg.Telegram.BotToken) != "" && cfg.Telegram.OwnerChatID != 0 && cfg.Telegram.OwnerUserID != 0
}

func launchConfigError(cfg config.Config) error {
	if strings.TrimSpace(cfg.Telegram.BotToken) == "" {
		return errors.New("telegram bot token is missing; run `ollamaclaw configure` (or `ollamaclaw telegram init`) first")
	}
	if cfg.Telegram.OwnerChatID == 0 || cfg.Telegram.OwnerUserID == 0 {
		return errors.New("telegram owner allowlist is missing; run `ollamaclaw configure` (or set --owner-id via `ollamaclaw telegram init`)")
	}
	return errors.New("launch configuration is incomplete")
}

func isInteractiveTerminal() bool {
	if strings.TrimSpace(os.Getenv("OLLAMACLAW_FORCE_NONINTERACTIVE")) != "" {
		return false
	}
	if strings.TrimSpace(os.Getenv("OLLAMACLAW_FORCE_INTERACTIVE")) != "" {
		return true
	}
	stdinInfo, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	stdoutInfo, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (stdinInfo.Mode()&os.ModeCharDevice) != 0 && (stdoutInfo.Mode()&os.ModeCharDevice) != 0
}

func normalizeOwnerIDs(ownerID, ownerChatID, ownerUserID int64) (int64, int64) {
	if ownerID != 0 {
		if ownerChatID == 0 {
			ownerChatID = ownerID
		}
		if ownerUserID == 0 {
			ownerUserID = ownerID
		}
	}
	if ownerChatID != 0 && ownerUserID == 0 {
		ownerUserID = ownerChatID
	}
	if ownerUserID != 0 && ownerChatID == 0 {
		ownerChatID = ownerUserID
	}
	return ownerChatID, ownerUserID
}

func acquireLaunchLock() (string, func(), error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", nil, err
	}
	lockPath := filepath.Join(dir, "launch.lock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return "", nil, fmt.Errorf("another local OllamaClaw launch is already running (lock: %s); stop the existing process and retry", lockPath)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	}
	release := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	return lockPath, release, nil
}

func findOtherLaunchProcesses(selfPID int) ([]string, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, err
	}
	return parseLaunchProcessConflicts(string(out), selfPID), nil
}

func parseLaunchProcessConflicts(psOutput string, selfPID int) []string {
	type proc struct {
		pid  int
		ppid int
		cmd  string
	}
	lines := strings.Split(psOutput, "\n")
	procs := make([]proc, 0, len(lines))
	ppidByPID := map[int]int{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid < 0 {
			continue
		}
		prefix := fields[0] + " " + fields[1]
		cmd := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		procs = append(procs, proc{pid: pid, ppid: ppid, cmd: cmd})
		ppidByPID[pid] = ppid
	}
	ancestorSet := map[int]struct{}{selfPID: {}}
	for p := selfPID; p > 1; {
		parent, ok := ppidByPID[p]
		if !ok || parent <= 1 {
			break
		}
		ancestorSet[parent] = struct{}{}
		p = parent
	}

	conflicts := make([]string, 0)
	for _, p := range procs {
		if _, isSelfOrAncestor := ancestorSet[p.pid]; isSelfOrAncestor {
			continue
		}
		cmd := p.cmd
		cmdLower := strings.ToLower(cmd)
		if !strings.Contains(cmdLower, "ollamaclaw") {
			continue
		}
		if strings.Contains(cmdLower, "pgrep") || strings.Contains(cmdLower, "grep ") {
			continue
		}
		isLaunchCandidate := strings.Contains(cmdLower, "ollamaclaw telegram run") ||
			strings.Contains(cmdLower, "ollamaclaw launch") ||
			strings.HasSuffix(cmdLower, "/ollamaclaw") ||
			strings.HasSuffix(cmdLower, " ./ollamaclaw") ||
			cmdLower == "./ollamaclaw"
		if !isLaunchCandidate {
			continue
		}
		conflicts = append(conflicts, fmt.Sprintf("pid=%d cmd=%q", p.pid, previewCommandForError(cmd)))
	}
	return conflicts
}

func previewCommandForError(cmd string) string {
	compact := strings.Join(strings.Fields(strings.TrimSpace(cmd)), " ")
	const max = 180
	if len(compact) <= max {
		return compact
	}
	return compact[:max-3] + "..."
}

type runtime struct {
	cfg           config.Config
	store         *db.Store
	engine        *agent.Engine
	pluginManager *plugin.Manager
	cron          *cronjobs.Manager
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
	cronMgr := cronjobs.NewManager(store)
	eng := agent.New(cfg, store, client, pm, cronMgr)
	cronMgr.SetRunner(func(ctx context.Context, transport, sessionKey, prompt string) (cronjobs.RunResult, error) {
		res, err := eng.HandleText(ctx, transport, sessionKey, prompt)
		if err != nil {
			return cronjobs.RunResult{}, err
		}
		return cronjobs.RunResult{
			Output:       res.AssistantContent,
			BashCommands: extractBashCommands(res.ToolTrace),
		}, nil
	})
	cleanup := func() {
		cronMgr.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = store.SetSetting(ctx, "last_shutdown", time.Now().UTC().Format(time.RFC3339Nano))
		_ = store.Close()
	}
	return &runtime{cfg: cfg, store: store, engine: eng, pluginManager: pm, cron: cronMgr}, cleanup, nil
}

func extractBashCommands(trace []agent.ToolTraceEntry) []string {
	out := make([]string, 0, len(trace))
	for _, entry := range trace {
		if !strings.EqualFold(strings.TrimSpace(entry.Name), "bash") {
			continue
		}
		if strings.TrimSpace(entry.ArgsJSON) == "" {
			continue
		}
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(entry.ArgsJSON), &args); err != nil {
			continue
		}
		command, _ := args["command"].(string)
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		out = append(out, command)
	}
	return cronjobs.BashCommandsFromTrace(out)
}
