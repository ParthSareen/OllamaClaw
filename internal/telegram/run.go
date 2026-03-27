package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ParthSareen/OllamaClaw/internal/agent"
	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/cronjobs"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Runner struct {
	Cfg       config.Config
	Store     *db.Store
	Engine    *agent.Engine
	Scheduler *cronjobs.Manager

	lastUpdateID atomic.Int64
	nextTurnID   atomic.Uint64
	logMu        sync.Mutex
	logFile      *os.File
	turnMu       sync.Mutex
	inFlight     map[string]inFlightTurn
	unauthMu     sync.Mutex
	unauthAt     map[string]time.Time
}

const (
	settingOffsetKey          = "telegram_last_update_id"
	maxLogPreview             = 280
	maxToolPreview            = 700
	updateWorkers             = 8
	unauthorizedReplyCooldown = time.Minute
)

type inFlightTurn struct {
	id     uint64
	chatID int64
	cancel context.CancelFunc
}

func (r *Runner) Run(ctx context.Context) error {
	if err := r.openLogFile(); err != nil {
		fmt.Printf("[%s] [launch] log sink setup failed: %v\n", time.Now().UTC().Format(time.RFC3339), err)
	}
	defer r.closeLogFile()

	offset := r.readOffset(ctx)
	r.lastUpdateID.Store(int64(offset))
	r.logf("launch starting: db=%s owner_chat_id=%d owner_user_id=%d initial_offset=%d", r.Cfg.DBPath, r.Cfg.Telegram.OwnerChatID, r.Cfg.Telegram.OwnerUserID, offset)

	if err := r.ensurePollingOwnership(ctx, offset); err != nil {
		r.logf("telegram polling preflight failed: %v", r.redactError(err))
		return err
	}
	r.logf("telegram polling preflight passed")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	opts := []bot.Option{
		bot.WithDefaultHandler(r.handleUpdate),
		// Keep updates concurrent so /stop can be processed while a long tool call is running.
		bot.WithWorkers(updateWorkers),
		bot.WithAllowedUpdates([]string{"message"}),
		bot.WithInitialOffset(int64(offset)),
		bot.WithErrorsHandler(func(err error) {
			if err == nil {
				return
			}
			if isPollingConflictErr(err) {
				hint := localPollerHint(os.Getpid())
				if hint == "" {
					hint = "no local ollamaclaw poller candidates detected"
				}
				r.logf("telegram long polling conflict detected; waiting and retrying (%s): %v", hint, r.redactError(err))
				return
			}
			r.logf("telegram polling error: %v", r.redactError(err))
		}),
	}
	b, err := bot.New(r.Cfg.Telegram.BotToken, opts...)
	if err != nil {
		r.logf("bot init failed: %v", r.redactError(err))
		return r.redactError(err)
	}
	r.logf("telegram client initialized (long polling, private chats)")
	if err := SyncCommands(runCtx, r.Cfg.Telegram.BotToken); err != nil {
		r.logf("telegram command sync warning: %v", r.redactError(err))
	} else {
		r.logf("telegram commands synced")
	}
	if r.Scheduler != nil {
		jobs, err := r.Scheduler.ListJobs(ctx, true)
		if err != nil {
			r.logf("cron preload list failed: %v", err)
		} else {
			r.logf("cron active jobs loaded: %d", len(jobs))
		}
		r.Scheduler.SetOutputSink(func(ctx context.Context, transport, sessionKey, content string) error {
			if transport != "telegram" {
				return nil
			}
			chatID, err := strconv.ParseInt(sessionKey, 10, 64)
			if err != nil {
				r.logf("cron output drop: invalid session_key=%q error=%v", sessionKey, r.redactError(err))
				return err
			}
			r.logf("cron output -> chat=%d bytes=%d preview=%q", chatID, len(content), r.previewForLog(content))
			r.sendChunked(ctx, b, chatID, nil, content)
			return nil
		})
		if err := r.Scheduler.Start(runCtx); err != nil {
			r.logf("cron scheduler start failed: %v", err)
			return err
		}
		r.logf("cron scheduler started")
		defer r.Scheduler.Stop()
		defer r.logf("cron scheduler stopped")
	}
	r.logf("telegram bot running")
	b.Start(runCtx)
	r.logf("telegram runner stopped")
	return nil
}

func (r *Runner) ensurePollingOwnership(ctx context.Context, offset int) error {
	_, err := call(ctx, r.Cfg.Telegram.BotToken, "getUpdates", map[string]interface{}{
		"offset":          int64(offset + 1),
		"limit":           1,
		"timeout":         0,
		"allowed_updates": []string{"message"},
	})
	if err == nil {
		return nil
	}
	if isPollingConflictErr(err) {
		return fmt.Errorf("another bot instance is currently polling this token")
	}
	return fmt.Errorf("telegram getUpdates preflight failed: %w", r.redactError(err))
}

func (r *Runner) readOffset(ctx context.Context) int {
	v, ok, err := r.Store.GetSetting(ctx, settingOffsetKey)
	if err != nil || !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

func (r *Runner) setOffset(ctx context.Context, updateID int64) {
	_ = r.Store.SetSetting(ctx, settingOffsetKey, strconv.FormatInt(updateID, 10))
	r.lastUpdateID.Store(updateID)
	r.logf("checkpoint offset=%d", updateID)
}

func (r *Runner) handleUpdate(ctx context.Context, b *bot.Bot, update *models.Update) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logf("panic in update handler: %v\n%s", rec, string(debug.Stack()))
		}
	}()

	if update == nil {
		r.logf("received nil update")
		return
	}
	r.logf("update received: id=%d", update.ID)
	if int64(update.ID) <= r.lastUpdateID.Load() {
		r.logf("update skipped: stale id=%d last=%d", update.ID, r.lastUpdateID.Load())
		return
	}
	r.setOffset(ctx, update.ID)

	if update.Message == nil {
		r.logf("update ignored: no message id=%d", update.ID)
		return
	}
	if update.Message.Chat.Type != models.ChatTypePrivate {
		r.logf("update ignored: chat_type=%s chat=%d", update.Message.Chat.Type, update.Message.Chat.ID)
		return
	}
	chatID := update.Message.Chat.ID
	var userID int64
	if update.Message.From != nil {
		userID = update.Message.From.ID
	}
	text := strings.TrimSpace(update.Message.Text)
	if text == "" {
		r.logf("message ignored: empty text chat=%d user=%d", chatID, userID)
		return
	}
	cmd := parseCommand(text)
	if !r.authorize(ctx, b, chatID, userID, cmd) {
		return
	}
	r.logf("authorization accepted: chat=%d user=%d command=%q", chatID, userID, cmd)
	r.logf("message received: chat=%d user=%d chars=%d preview=%q", chatID, userID, len(text), r.previewForLog(text))
	if cmd != "" {
		r.logf("command dispatch: chat=%d cmd=%s raw=%q", chatID, cmd, r.previewForLog(text))
		r.handleCommand(ctx, b, chatID, text)
		return
	}

	sessionKey := strconv.FormatInt(chatID, 10)
	turnCtx, turnCancel := context.WithCancel(ctx)
	turnID, started := r.beginTurn(sessionKey, chatID, turnCancel)
	if !started {
		turnCancel()
		r.logf("agent turn rejected: chat=%d session_key=%s reason=in_progress", chatID, sessionKey)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "a turn is already running. send /stop to interrupt it first."})
		return
	}
	defer r.endTurn(sessionKey, turnID)

	progress, _ := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "Thinking..."})
	if progress != nil {
		r.logf("progress message sent: chat=%d message_id=%d", chatID, progress.ID)
	}
	r.logf("agent turn start: chat=%d session_key=%s", chatID, sessionKey)
	startedAt := time.Now()
	showTools, _ := r.Engine.IsSessionShowTools(ctx, "telegram", sessionKey)
	if showTools {
		r.logf("live tool stream enabled: chat=%d session_key=%s", chatID, sessionKey)
	}
	res, err := r.Engine.HandleTextWithOptions(turnCtx, "telegram", sessionKey, text, agent.HandleOptions{
		OnToolEvent: func(ev agent.ToolEvent) {
			if !showTools || ev.Phase != agent.ToolEventFinish {
				return
			}
			line := formatLiveToolEvent(ev)
			r.logf("tool event: chat=%d %s", chatID, r.previewForLog(line))
			sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, sendErr := b.SendMessage(sendCtx, &bot.SendMessageParams{ChatID: chatID, Text: line})
			if sendErr != nil {
				r.logf("tool event send failed: chat=%d error=%v", chatID, r.redactError(sendErr))
			}
		},
	})
	if err != nil {
		if isContextCanceledErr(err) {
			r.logf("agent turn canceled: chat=%d session_key=%s elapsed_ms=%d", chatID, sessionKey, time.Since(startedAt).Milliseconds())
			if progress != nil {
				_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: progress.ID, Text: "stopped."})
			} else {
				_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "stopped."})
			}
			return
		}
		r.logf("agent turn failed: chat=%d error=%v", chatID, r.redactError(err))
		r.replyError(ctx, b, chatID, progress, err)
		return
	}
	r.logf("agent turn complete: chat=%d model=%s prompt_tokens=%d eval_tokens=%d compacted=%t tool_calls=%d elapsed_ms=%d", chatID, res.Session.ModelOverride, res.PromptTokens, res.EvalTokens, res.Compacted, len(res.ToolTrace), time.Since(startedAt).Milliseconds())
	for i, tr := range res.ToolTrace {
		line := fmt.Sprintf("tool trace [%d/%d]: chat=%d name=%s duration_ms=%d args=%q", i+1, len(res.ToolTrace), chatID, tr.Name, tr.DurationMs, r.previewForLog(tr.ArgsJSON))
		if strings.TrimSpace(tr.Error) != "" {
			line += fmt.Sprintf(" error=%q", r.previewForLog(tr.Error))
		} else {
			line += fmt.Sprintf(" result=%q", r.previewForLog(tr.ResultJSON))
		}
		r.logf("%s", line)
	}
	verbose, _ := r.Engine.IsSessionVerbose(ctx, "telegram", sessionKey)
	if verbose && len(res.ToolTrace) > 0 {
		trace := formatToolTrace(res.ToolTrace)
		if strings.TrimSpace(res.AssistantContent) == "" {
			res.AssistantContent = trace
		} else {
			res.AssistantContent += "\n\n" + trace
		}
	}
	if strings.TrimSpace(res.AssistantContent) == "" {
		res.AssistantContent = "(empty response)"
	}
	r.logf("response send: chat=%d chars=%d", chatID, len(res.AssistantContent))
	if showTools {
		if progress != nil {
			deleted, err := b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: progress.ID})
			if err != nil || !deleted {
				r.logf("delete progress failed: chat=%d message_id=%d deleted=%t error=%v", chatID, progress.ID, deleted, r.redactError(err))
			} else {
				r.logf("progress message deleted: chat=%d message_id=%d", chatID, progress.ID)
			}
		}
		r.sendChunked(ctx, b, chatID, nil, res.AssistantContent)
		return
	}
	r.sendChunked(ctx, b, chatID, progress, res.AssistantContent)
}

func (r *Runner) authorize(ctx context.Context, b *bot.Bot, chatID, userID int64, cmd string) bool {
	send := func(text string) {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
	}

	ownerChatID := r.Cfg.Telegram.OwnerChatID
	ownerUserID := r.Cfg.Telegram.OwnerUserID
	if ownerChatID == chatID && ownerUserID == userID {
		return true
	}
	shouldReply := r.shouldSendUnauthorizedReply(chatID, userID, time.Now())
	if cmd == "start" {
		r.logf("unauthorized /start attempt: chat=%d user=%d reply=%t", chatID, userID, shouldReply)
		if shouldReply {
			send("Unauthorized DM. This bot is restricted to the server allowlist.")
		}
		return false
	}
	r.logf("unauthorized message: chat=%d user=%d command=%q reply=%t", chatID, userID, cmd, shouldReply)
	if shouldReply {
		send("Unauthorized DM. This bot only accepts messages from the allowlisted owner.")
	}
	return false
}

func parseCommand(raw string) string {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return ""
	}
	token := parts[0]
	if token == "/" && len(parts) > 1 {
		token = "/" + parts[1]
	}
	cmd := strings.ToLower(strings.TrimPrefix(token, "/"))
	if cmd == "" {
		return ""
	}
	if at := strings.Index(cmd, "@"); at > 0 {
		cmd = cmd[:at]
	}
	return cmd
}

func (r *Runner) replyError(ctx context.Context, b *bot.Bot, chatID int64, progress *models.Message, err error) {
	err = r.redactError(err)
	msg := "error: " + err.Error()
	r.logf("reply error: chat=%d message=%q", chatID, r.previewForLog(msg))
	if progress != nil {
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: progress.ID, Text: msg})
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: msg})
}

func (r *Runner) handleCommand(ctx context.Context, b *bot.Bot, chatID int64, raw string) {
	parts := strings.Fields(strings.TrimSpace(raw))
	if len(parts) == 0 {
		return
	}
	if parts[0] == "/" {
		parts = parts[1:]
		if len(parts) == 0 {
			return
		}
	}
	cmd := parseCommand(raw)

	send := func(text string) {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
	}
	sendErr := func(err error) {
		err = r.redactError(err)
		send("error: " + err.Error())
	}

	sessionKey := strconv.FormatInt(chatID, 10)
	sess, err := r.Engine.GetOrCreateSession(ctx, "telegram", sessionKey)
	if err != nil {
		sendErr(err)
		return
	}

	switch cmd {
	case "start":
		r.logf("command start: chat=%d", chatID)
		send("OllamaClaw is ready.\nCommands:\n/start\n/help\n/model [name]\n/tools\n/show tools [on|off]\n/show thinking [on|off]\n/verbose [on|off]\n/think [on|off]\n/status\n/reset\n/stop")
	case "help":
		r.logf("command help: chat=%d", chatID)
		send("Commands:\n/start\n/help\n/model [name]\n/tools\n/show tools [on|off]\n/show thinking [on|off]\n/verbose [on|off]\n/think [on|off]\n/status\n/reset\n/stop\n\nSend any text to chat with OllamaClaw.")
	case "reset":
		r.logf("command reset: chat=%d", chatID)
		newSess, err := r.Engine.ResetSession(ctx, "telegram", sessionKey)
		if err != nil {
			r.logf("command reset failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		r.logf("command reset complete: chat=%d new_session=%s", chatID, newSess.ID)
		send("session reset: " + newSess.ID)
	case "model":
		r.logf("command model: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if len(parts) == 1 {
			send("model: " + redactTelegramToken(r.Cfg.Telegram.BotToken, sess.ModelOverride))
			return
		}
		model := strings.TrimSpace(strings.Join(parts[1:], " "))
		if model == "" {
			send("usage: /model <name>")
			return
		}
		if err := r.Engine.SetSessionModel(ctx, sess.ID, model); err != nil {
			r.logf("command model failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		r.logf("command model set: chat=%d model=%s", chatID, redactTelegramToken(r.Cfg.Telegram.BotToken, model))
		send("model set to: " + redactTelegramToken(r.Cfg.Telegram.BotToken, model))
	case "tools":
		r.logf("command tools: chat=%d", chatID)
		all, err := r.Engine.ListTools(ctx)
		if err != nil {
			r.logf("command tools failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		lines := []string{"tools:"}
		for _, t := range all {
			if t.Source == "plugin" {
				lines = append(lines, fmt.Sprintf("- %s (plugin:%s)", t.Name, t.PluginID))
			} else {
				lines = append(lines, "- "+t.Name)
			}
		}
		send(strings.Join(lines, "\n"))
	case "verbose":
		r.logf("command verbose: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if len(parts) == 1 {
			enabled, err := r.Engine.IsSessionVerbose(ctx, "telegram", sessionKey)
			if err != nil {
				r.logf("command verbose read failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			send(fmt.Sprintf("verbose: %t", enabled))
			return
		}
		enabled, ok := parseOnOff(parts[1])
		if !ok {
			send("usage: /verbose [on|off]")
			return
		}
		if err := r.Engine.SetSessionVerbose(ctx, "telegram", sessionKey, enabled); err != nil {
			r.logf("command verbose set failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		r.logf("command verbose set: chat=%d enabled=%t", chatID, enabled)
		send(fmt.Sprintf("verbose: %t", enabled))
	case "show":
		r.logf("command show: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if len(parts) < 2 {
			send("usage: /show <tools|thinking> [on|off]")
			return
		}
		target := strings.ToLower(strings.TrimSpace(parts[1]))
		switch target {
		case "tools":
			if len(parts) == 2 {
				if err := r.Engine.SetSessionShowTools(ctx, "telegram", sessionKey, true); err != nil {
					r.logf("command show tools set failed: chat=%d error=%v", chatID, r.redactError(err))
					sendErr(err)
					return
				}
				send("show tools: true")
				return
			}
			enabled, ok := parseOnOff(parts[2])
			if !ok {
				send("usage: /show tools [on|off]")
				return
			}
			if err := r.Engine.SetSessionShowTools(ctx, "telegram", sessionKey, enabled); err != nil {
				r.logf("command show tools set failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			r.logf("command show tools set: chat=%d enabled=%t", chatID, enabled)
			send(fmt.Sprintf("show tools: %t", enabled))
		case "thinking", "think":
			if len(parts) == 2 {
				if err := r.Engine.SetSessionThink(ctx, "telegram", sessionKey, true); err != nil {
					r.logf("command show thinking set failed: chat=%d error=%v", chatID, r.redactError(err))
					sendErr(err)
					return
				}
				send("thinking: true")
				return
			}
			enabled, ok := parseOnOff(parts[2])
			if !ok {
				send("usage: /show thinking [on|off]")
				return
			}
			if err := r.Engine.SetSessionThink(ctx, "telegram", sessionKey, enabled); err != nil {
				r.logf("command show thinking set failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			r.logf("command show thinking set: chat=%d enabled=%t", chatID, enabled)
			send(fmt.Sprintf("thinking: %t", enabled))
		default:
			send("usage: /show <tools|thinking> [on|off]")
		}
	case "think":
		r.logf("command think: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if len(parts) == 1 {
			enabled, err := r.Engine.IsSessionThink(ctx, "telegram", sessionKey)
			if err != nil {
				r.logf("command think read failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			send(fmt.Sprintf("think: %t", enabled))
			return
		}
		enabled, ok := parseOnOff(parts[1])
		if !ok {
			send("usage: /think [on|off]")
			return
		}
		if err := r.Engine.SetSessionThink(ctx, "telegram", sessionKey, enabled); err != nil {
			r.logf("command think set failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		r.logf("command think set: chat=%d enabled=%t", chatID, enabled)
		send(fmt.Sprintf("think: %t", enabled))
	case "status":
		r.logf("command status: chat=%d", chatID)
		enabledPlugins, _ := r.Store.ListPlugins(ctx, true)
		verbose, _ := r.Engine.IsSessionVerbose(ctx, "telegram", sessionKey)
		showTools, _ := r.Engine.IsSessionShowTools(ctx, "telegram", sessionKey)
		think, _ := r.Engine.IsSessionThink(ctx, "telegram", sessionKey)
		text := fmt.Sprintf("status:\nmodel: %s\nverbose: %t\nshow_tools: %t\nthink: %t\nprompt_tokens: %d\ncompletion_tokens: %d\ncompactions: %d\nenabled_plugins: %d\ndb: %s\nlog: %s", redactTelegramToken(r.Cfg.Telegram.BotToken, sess.ModelOverride), verbose, showTools, think, sess.TotalPromptToken, sess.TotalEvalToken, sess.CompactionCount, len(enabledPlugins), r.Cfg.DBPath, strings.TrimSpace(r.Cfg.LogPath))
		send(text)
	case "stop":
		r.logf("command stop: chat=%d", chatID)
		turn, ok := r.stopTurn(sessionKey)
		if !ok {
			send("no active turn to stop")
			return
		}
		r.logf("command stop signaled: chat=%d session_key=%s turn_id=%d", chatID, sessionKey, turn.id)
		send("stopping current turn...")
	default:
		r.logf("unknown command: chat=%d cmd=%s", chatID, cmd)
		send("unknown command")
	}
}

func (r *Runner) beginTurn(sessionKey string, chatID int64, cancel context.CancelFunc) (uint64, bool) {
	r.turnMu.Lock()
	defer r.turnMu.Unlock()
	if r.inFlight == nil {
		r.inFlight = map[string]inFlightTurn{}
	}
	if _, exists := r.inFlight[sessionKey]; exists {
		return 0, false
	}
	id := r.nextTurnID.Add(1)
	r.inFlight[sessionKey] = inFlightTurn{
		id:     id,
		chatID: chatID,
		cancel: cancel,
	}
	return id, true
}

func (r *Runner) endTurn(sessionKey string, id uint64) {
	r.turnMu.Lock()
	defer r.turnMu.Unlock()
	turn, ok := r.inFlight[sessionKey]
	if !ok || turn.id != id {
		return
	}
	delete(r.inFlight, sessionKey)
}

func (r *Runner) stopTurn(sessionKey string) (inFlightTurn, bool) {
	r.turnMu.Lock()
	turn, ok := r.inFlight[sessionKey]
	r.turnMu.Unlock()
	if !ok {
		return inFlightTurn{}, false
	}
	turn.cancel()
	return turn, true
}

func (r *Runner) sendChunked(ctx context.Context, b *bot.Bot, chatID int64, progress *models.Message, text string) {
	chunks := splitText(text, 3900)
	if len(chunks) == 0 {
		chunks = []string{"(empty response)"}
	}

	r.logf("sendChunked: chat=%d chunks=%d first_chunk_chars=%d", chatID, len(chunks), len(chunks[0]))
	if progress != nil {
		_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: progress.ID, Text: chunks[0]})
		if err != nil {
			r.logf("edit progress failed, fallback send: chat=%d message_id=%d error=%v", chatID, progress.ID, r.redactError(err))
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: chunks[0]})
		}
	} else {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: chunks[0]})
	}
	for i := 1; i < len(chunks); i++ {
		r.logf("sendChunked: chat=%d chunk=%d/%d chars=%d", chatID, i+1, len(chunks), len(chunks[i]))
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: chunks[i]})
	}
}

func splitText(text string, max int) []string {
	if max <= 0 || len(text) <= max {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	}
	out := []string{}
	for len(text) > max {
		splitAt := strings.LastIndex(text[:max], "\n")
		if splitAt < max/2 {
			splitAt = max
		}
		out = append(out, text[:splitAt])
		text = strings.TrimLeft(text[splitAt:], "\n")
	}
	if strings.TrimSpace(text) != "" {
		out = append(out, text)
	}
	return out
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

func formatLiveToolEvent(ev agent.ToolEvent) string {
	if ev.Phase == agent.ToolEventStart {
		return fmt.Sprintf("tool start %d: %s args=%s", ev.Index, ev.Name, truncateForLive(ev.ArgsJSON))
	}
	if strings.TrimSpace(ev.Error) != "" {
		return fmt.Sprintf("tool done %d: %s (%d ms) error=%s", ev.Index, ev.Name, ev.DurationMs, truncateForLive(ev.Error))
	}
	return fmt.Sprintf("tool done %d: %s (%d ms) result=%s", ev.Index, ev.Name, ev.DurationMs, truncateForLive(ev.ResultJSON))
}

func truncateForLive(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "{}"
	}
	if len(v) <= maxToolPreview {
		return v
	}
	return v[:maxToolPreview-3] + "..."
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

func isContextCanceledErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "context canceled")
}

func isPollingConflictErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, bot.ErrorConflict) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") && strings.Contains(msg, "getupdates")
}

type pollerCandidate struct {
	pid int
	cmd string
}

func localPollerHint(selfPID int) string {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return ""
	}
	candidates := parsePollerCandidates(string(out), selfPID)
	if len(candidates) == 0 {
		return ""
	}
	max := 3
	if len(candidates) < max {
		max = len(candidates)
	}
	parts := make([]string, 0, max)
	for i := 0; i < max; i++ {
		c := candidates[i]
		parts = append(parts, fmt.Sprintf("pid=%d cmd=%q", c.pid, previewForLog(c.cmd)))
	}
	return strings.Join(parts, "; ")
}

func parsePollerCandidates(psOutput string, selfPID int) []pollerCandidate {
	lines := strings.Split(psOutput, "\n")
	out := make([]pollerCandidate, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 || pid == selfPID {
			continue
		}
		cmd := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		cmdLower := strings.ToLower(cmd)
		if strings.Contains(cmdLower, "ollamaclaw") {
			// Focus on launch-style invocations that can poll Telegram.
			if strings.Contains(cmdLower, "telegram run") || strings.Contains(cmdLower, " launch") || strings.HasSuffix(cmdLower, "/ollamaclaw") || strings.HasSuffix(cmdLower, " ./ollamaclaw") || cmdLower == "./ollamaclaw" {
				out = append(out, pollerCandidate{pid: pid, cmd: cmd})
			}
			continue
		}
		// Include known external Telegram bot runner patterns for easier diagnosis.
		if strings.Contains(cmdLower, "telegram") && (strings.Contains(cmdLower, "plugins-official/telegram") || strings.Contains(cmdLower, " bot") || strings.Contains(cmdLower, " getupdates ")) {
			out = append(out, pollerCandidate{pid: pid, cmd: cmd})
		}
	}
	return out
}

func (r *Runner) logf(format string, args ...interface{}) {
	ts := time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("[%s] [launch] %s", ts, fmt.Sprintf(format, args...))
	fmt.Println(line)
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if r.logFile != nil {
		_, _ = r.logFile.WriteString(line + "\n")
	}
}

func (r *Runner) redactError(err error) error {
	return redactTelegramError(r.Cfg.Telegram.BotToken, err)
}

func (r *Runner) previewForLog(s string) string {
	return previewForLog(redactTelegramToken(r.Cfg.Telegram.BotToken, s))
}

func (r *Runner) shouldSendUnauthorizedReply(chatID, userID int64, now time.Time) bool {
	key := fmt.Sprintf("%d:%d", chatID, userID)
	r.unauthMu.Lock()
	defer r.unauthMu.Unlock()
	if r.unauthAt == nil {
		r.unauthAt = map[string]time.Time{}
	}
	last, ok := r.unauthAt[key]
	if ok && now.Sub(last) < unauthorizedReplyCooldown {
		return false
	}
	r.unauthAt[key] = now
	return true
}

func (r *Runner) openLogFile() error {
	path := strings.TrimSpace(r.Cfg.LogPath)
	if path == "" {
		return nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	r.logMu.Lock()
	r.logFile = f
	r.logMu.Unlock()
	return nil
}

func (r *Runner) closeLogFile() {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if r.logFile != nil {
		_ = r.logFile.Close()
		r.logFile = nil
	}
}

func previewForLog(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	compact := strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(compact) <= maxLogPreview {
		return compact
	}
	return compact[:maxLogPreview-3] + "..."
}
