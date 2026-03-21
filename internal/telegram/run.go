package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/parth/ollamaclaw/internal/agent"
	"github.com/parth/ollamaclaw/internal/config"
	"github.com/parth/ollamaclaw/internal/db"
)

type Runner struct {
	Cfg    config.Config
	Store  *db.Store
	Engine *agent.Engine

	lastUpdateID atomic.Int64
}

func (r *Runner) Run(ctx context.Context) error {
	offset := r.readOffset(ctx)
	r.lastUpdateID.Store(int64(offset))

	opts := []bot.Option{
		bot.WithDefaultHandler(r.handleUpdate),
		bot.WithAllowedUpdates([]string{"message"}),
		bot.WithInitialOffset(int64(offset + 1)),
	}
	b, err := bot.New(r.Cfg.Telegram.BotToken, opts...)
	if err != nil {
		return err
	}
	fmt.Println("Telegram bot running (private chats only)")
	b.Start(ctx)
	return nil
}

func (r *Runner) readOffset(ctx context.Context) int {
	v, ok, err := r.Store.GetSetting(ctx, "telegram_last_update_id")
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
	_ = r.Store.SetSetting(ctx, "telegram_last_update_id", strconv.FormatInt(updateID, 10))
	r.lastUpdateID.Store(updateID)
}

func (r *Runner) handleUpdate(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update == nil {
		return
	}
	if int64(update.ID) <= r.lastUpdateID.Load() {
		return
	}
	r.setOffset(ctx, update.ID)

	if update.Message == nil {
		return
	}
	if update.Message.Chat.Type != models.ChatTypePrivate {
		return
	}
	chatID := update.Message.Chat.ID
	text := strings.TrimSpace(update.Message.Text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		r.handleCommand(ctx, b, chatID, text)
		return
	}

	progress, _ := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: "Thinking..."})
	res, err := r.Engine.HandleText(ctx, "telegram", strconv.FormatInt(chatID, 10), text)
	if err != nil {
		r.replyError(ctx, b, chatID, progress, err)
		return
	}
	if strings.TrimSpace(res.AssistantContent) == "" {
		res.AssistantContent = "(empty response)"
	}
	r.sendChunked(ctx, b, chatID, progress, res.AssistantContent)
}

func (r *Runner) replyError(ctx context.Context, b *bot.Bot, chatID int64, progress *models.Message, err error) {
	msg := "error: " + err.Error()
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
	cmd := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	if at := strings.Index(cmd, "@"); at > 0 {
		cmd = cmd[:at]
	}

	send := func(text string) {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
	}

	sessionKey := strconv.FormatInt(chatID, 10)
	sess, err := r.Engine.GetOrCreateSession(ctx, "telegram", sessionKey)
	if err != nil {
		send("error: " + err.Error())
		return
	}

	switch cmd {
	case "help":
		send("Commands:\n/help\n/model [name]\n/tools\n/status\n/reset\n\nSend any text to chat with OllamaClaw.")
	case "reset":
		newSess, err := r.Engine.ResetSession(ctx, "telegram", sessionKey)
		if err != nil {
			send("error: " + err.Error())
			return
		}
		send("session reset: " + newSess.ID)
	case "model":
		if len(parts) == 1 {
			send("model: " + sess.ModelOverride)
			return
		}
		model := strings.TrimSpace(strings.Join(parts[1:], " "))
		if model == "" {
			send("usage: /model <name>")
			return
		}
		if err := r.Engine.SetSessionModel(ctx, sess.ID, model); err != nil {
			send("error: " + err.Error())
			return
		}
		send("model set to: " + model)
	case "tools":
		all, err := r.Engine.ListTools(ctx)
		if err != nil {
			send("error: " + err.Error())
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
	case "status":
		enabledPlugins, _ := r.Store.ListPlugins(ctx, true)
		text := fmt.Sprintf("status:\nmodel: %s\nprompt_tokens: %d\ncompletion_tokens: %d\ncompactions: %d\nenabled_plugins: %d\ndb: %s", sess.ModelOverride, sess.TotalPromptToken, sess.TotalEvalToken, sess.CompactionCount, len(enabledPlugins), r.Cfg.DBPath)
		send(text)
	default:
		send("unknown command")
	}
}

func (r *Runner) sendChunked(ctx context.Context, b *bot.Bot, chatID int64, progress *models.Message, text string) {
	chunks := splitText(text, 3900)
	if len(chunks) == 0 {
		chunks = []string{"(empty response)"}
	}
	if progress != nil {
		_, err := b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: progress.ID, Text: chunks[0]})
		if err != nil {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: chunks[0]})
		}
	} else {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: chunks[0]})
	}
	for i := 1; i < len(chunks); i++ {
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
