package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	"github.com/ParthSareen/OllamaClaw/internal/audio"
	"github.com/ParthSareen/OllamaClaw/internal/config"
	"github.com/ParthSareen/OllamaClaw/internal/cronjobs"
	"github.com/ParthSareen/OllamaClaw/internal/db"
	"github.com/ParthSareen/OllamaClaw/internal/subagents"
	"github.com/ParthSareen/OllamaClaw/internal/tools"
	"github.com/ParthSareen/OllamaClaw/internal/util"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Runner struct {
	Cfg        config.Config
	Store      *db.Store
	Engine     *agent.Engine
	Scheduler  *cronjobs.Manager
	Subagents  *subagents.Manager
	AppVersion string

	lastUpdateID atomic.Int64
	nextTurnID   atomic.Uint64
	restarting   atomic.Bool
	logMu        sync.Mutex
	logFile      *os.File
	runMu        sync.Mutex
	runCancel    context.CancelFunc
	turnMu       sync.Mutex
	inFlight     map[string]inFlightTurn
	pendingMu    sync.Mutex
	pendingTurns map[string]pendingTurn
	nextPending  atomic.Uint64
	nextApproval atomic.Uint64
	approvalMu   sync.Mutex
	approvals    map[string]*pendingApproval
	webhookMu    sync.Mutex
	unauthMu     sync.Mutex
	unauthAt     map[string]time.Time
	voiceMu      sync.Mutex
	voice        *audio.Service
	turnExecutor func(ctx context.Context, turnCtx context.Context, b *bot.Bot, chatID, userID int64, sessionKey, text string, imageFileIDs []string)
}

const (
	settingOffsetKey                  = "telegram_last_update_id"
	settingTelegramBashAlwaysAllowKey = "telegram_bash_always_allow"
	settingTelegramVoiceReplyModeKey  = "telegram_voice_reply_mode"
	settingTelegramVoiceOutputModeKey = "telegram_voice_output_mode"
	maxLogPreview                     = 280
	maxToolPreview                    = 700
	maxTelegramInputImages            = 4
	telegramImageMaxBytes             = 8 * 1024 * 1024
	telegramVoiceMaxBytes             = 20 * 1024 * 1024
	telegramImageFetchTimeout         = 20 * time.Second
	telegramVoiceFetchTimeout         = 60 * time.Second
	pendingTurnDebounce               = 1500 * time.Millisecond
	pendingTurnRetry                  = 100 * time.Millisecond
	updateWorkers                     = 8
	approvalTTL                       = 10 * time.Minute
	maxApprovalCommandPreview         = 300
	approvalCallbackPrefix            = "appr"
	unauthorizedReplyCooldown         = time.Minute
	startupFlushBatchLimit            = 100
	defaultVoiceReplyMode             = "both"
	defaultVoiceOutputMode            = "mac"
)

var ErrRestartRequested = errors.New("telegram restart requested")
var telegramAPICall = call

type inFlightTurn struct {
	id     uint64
	chatID int64
	cancel context.CancelFunc
}

type pendingTurn struct {
	generation   uint64
	chatID       int64
	userID       int64
	sessionKey   string
	text         string
	imageFileIDs []string
	voiceFileID  string
	localSource  string
	localAudio   string
	localCleanup func()
	noDebounce   bool
	messageCount int
	readyAt      time.Time
	bot          *bot.Bot
}

type turnExecutionOptions struct {
	LocalSource        string
	LocalAudio         string
	LocalCleanup       func()
	TelegramVoiceInput bool
}

func (o turnExecutionOptions) isLocalText() bool {
	return strings.TrimSpace(o.LocalSource) != "" && strings.TrimSpace(o.LocalAudio) == ""
}

func (o turnExecutionOptions) isLocalVoice() bool {
	if strings.TrimSpace(o.LocalAudio) != "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(o.LocalSource), localControlSourceHotkeyVoice)
}

type responseDelivery struct {
	SendText      bool
	TelegramVoice bool
	LocalSpeech   bool
}

type pendingApproval struct {
	ID          string
	ChatID      int64
	UserID      int64
	SessionKey  string
	Command     string
	Normalized  string
	Reason      string
	AllowAlways bool
	CreatedAt   time.Time
	ExpiresAt   time.Time
	MessageID   int
	DecisionCh  chan approvalDecision
}

type approvalDecision int

const (
	approvalDecisionDeny approvalDecision = iota
	approvalDecisionAllow
	approvalDecisionAllowAlways
)

type telegramBashApprover struct {
	r          *Runner
	bot        *bot.Bot
	chatID     int64
	userID     int64
	sessionKey string
}

type telegramFileClient interface {
	GetFile(ctx context.Context, params *bot.GetFileParams) (*models.File, error)
	FileDownloadLink(file *models.File) string
}

func (a *telegramBashApprover) ApproveBashCommand(ctx context.Context, req tools.BashApprovalRequest) error {
	return a.r.requestBashApproval(ctx, a.bot, a.chatID, a.userID, a.sessionKey, req.Command, req.Normalized, req.Reason, req.AllowAlways)
}

func (r *Runner) Run(ctx context.Context) error {
	if err := r.openLogFile(); err != nil {
		fmt.Printf("[%s] [launch] log sink setup failed: %v\n", time.Now().UTC().Format(time.RFC3339), err)
	}
	defer r.closeLogFile()

	offset := r.readOffset(ctx)
	r.lastUpdateID.Store(int64(offset))
	version := strings.TrimSpace(r.AppVersion)
	if version == "" {
		version = "dev"
	}
	r.logf("launch starting: version=%s db=%s owner_chat_id=%d owner_user_id=%d initial_offset=%d", version, r.Cfg.DBPath, r.Cfg.Telegram.OwnerChatID, r.Cfg.Telegram.OwnerUserID, offset)

	if err := r.ensurePollingOwnership(ctx, offset); err != nil {
		r.logf("telegram polling preflight failed: %v", r.redactError(err))
		return err
	}
	r.logf("telegram polling preflight passed")
	flushedOffset, flushedCount, flushErr := r.flushQueuedUpdates(ctx, offset)
	if flushErr != nil {
		r.logf("startup backlog flush skipped: %v", r.redactError(flushErr))
	} else if flushedOffset > offset {
		offset = flushedOffset
		r.setOffset(ctx, int64(offset))
		r.logf("startup backlog flush complete: dropped_updates=%d new_offset=%d", flushedCount, offset)
	} else {
		r.logf("startup backlog flush complete: no queued updates")
	}

	runCtx, cancel := context.WithCancel(ctx)
	r.restarting.Store(false)
	r.setRunCancel(cancel)
	defer cancel()
	defer r.setRunCancel(nil)

	opts := []bot.Option{
		bot.WithDefaultHandler(r.handleUpdate),
		// Keep updates concurrent so /stop can be processed while a long tool call is running.
		bot.WithWorkers(updateWorkers),
		bot.WithAllowedUpdates([]string{"message", "callback_query"}),
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
	r.Engine.SetCoreMemoryEventSink(func(ev agent.CoreMemoryEvent) {
		if !strings.EqualFold(strings.TrimSpace(ev.Transport), "telegram") {
			return
		}
		sessionKey := strings.TrimSpace(ev.SessionKey)
		if sessionKey == "" {
			return
		}
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 3*time.Second)
		enabled, err := r.Engine.IsSessionDreamingNotifications(checkCtx, "telegram", sessionKey)
		checkCancel()
		if err != nil {
			r.logf("dreaming event toggle check failed: session_key=%s error=%v", sessionKey, r.redactError(err))
			return
		}
		if !enabled {
			return
		}
		chatID, err := strconv.ParseInt(sessionKey, 10, 64)
		if err != nil {
			r.logf("dreaming event drop: invalid session_key=%q error=%v", sessionKey, r.redactError(err))
			return
		}
		text := formatCoreMemoryEvent(ev)
		r.logf("dreaming event: chat=%d %s", chatID, r.previewForLog(text))
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sendCancel()
		if _, err := b.SendMessage(sendCtx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
			r.logf("dreaming event send failed: chat=%d error=%v", chatID, r.redactError(err))
		}
	})
	defer r.Engine.SetCoreMemoryEventSink(nil)
	stopWebhook, webhookErr := r.startGitHubWebhookServer(runCtx, b)
	if webhookErr != nil {
		r.logf("github webhook server start failed: %v", r.redactError(webhookErr))
		return webhookErr
	}
	defer stopWebhook()
	stopLocalControl, localControlErr := r.startLocalControlServer(runCtx, b)
	if localControlErr != nil {
		r.logf("local control server start failed: %v", r.redactError(localControlErr))
		return localControlErr
	}
	defer stopLocalControl()
	r.logf("telegram client initialized (long polling, private chats)")
	if err := SyncCommands(runCtx, r.Cfg.Telegram.BotToken); err != nil {
		r.logf("telegram command sync warning: %v", r.redactError(err))
	} else {
		r.logf("telegram commands synced")
	}
	if r.Scheduler != nil {
		reminders, err := r.Scheduler.ListReminders(ctx, true)
		if err != nil {
			r.logf("reminder preload list failed: %v", err)
		} else {
			r.logf("active reminders loaded: %d", len(reminders))
		}
		r.Scheduler.SetOutputSink(func(ctx context.Context, transport, sessionKey, content string) error {
			if transport != "telegram" {
				return nil
			}
			chatID, err := strconv.ParseInt(sessionKey, 10, 64)
			if err != nil {
				r.logf("reminder output drop: invalid session_key=%q error=%v", sessionKey, r.redactError(err))
				return err
			}
			r.logf("reminder output -> chat=%d bytes=%d preview=%q", chatID, len(content), r.previewForLog(content))
			r.sendChunked(ctx, b, chatID, nil, content)
			return nil
		})
		if err := r.Scheduler.Start(runCtx); err != nil {
			r.logf("reminder scheduler start failed: %v", err)
			return err
		}
		r.logf("reminder scheduler started")
		defer r.Scheduler.Stop()
		defer r.logf("reminder scheduler stopped")
	}
	if r.Subagents != nil {
		r.Subagents.SetOutputSink(func(ctx context.Context, transport, sessionKey, content string) error {
			if transport != "telegram" {
				return nil
			}
			chatID, err := strconv.ParseInt(sessionKey, 10, 64)
			if err != nil {
				r.logf("subagent output drop: invalid session_key=%q error=%v", sessionKey, r.redactError(err))
				return err
			}
			r.logf("subagent output -> chat=%d bytes=%d preview=%q", chatID, len(content), r.previewForLog(content))
			r.sendChunked(ctx, b, chatID, nil, content)
			return nil
		})
		if err := r.Subagents.Start(runCtx); err != nil {
			r.logf("subagent manager start failed: %v", r.redactError(err))
			return err
		}
		r.logf("subagent manager started")
		defer r.Subagents.Stop()
		defer r.logf("subagent manager stopped")
	}
	r.logf("telegram bot running")
	b.Start(runCtx)
	if r.restarting.Load() {
		r.logf("telegram runner stopped: restart requested")
		return ErrRestartRequested
	}
	r.logf("telegram runner stopped")
	return nil
}

func (r *Runner) ensurePollingOwnership(ctx context.Context, offset int) error {
	_, err := telegramAPICall(ctx, r.Cfg.Telegram.BotToken, "getUpdates", map[string]interface{}{
		"offset":          int64(offset + 1),
		"limit":           1,
		"timeout":         0,
		"allowed_updates": []string{"message", "callback_query"},
	})
	if err == nil {
		return nil
	}
	if isPollingConflictErr(err) {
		return fmt.Errorf("another bot instance is currently polling this token")
	}
	return fmt.Errorf("telegram getUpdates preflight failed: %w", r.redactError(err))
}

func (r *Runner) flushQueuedUpdates(ctx context.Context, offset int) (latestOffset int, dropped int, err error) {
	latestOffset = offset
	for {
		raw, callErr := telegramAPICall(ctx, r.Cfg.Telegram.BotToken, "getUpdates", map[string]interface{}{
			"offset":          int64(latestOffset + 1),
			"limit":           startupFlushBatchLimit,
			"timeout":         0,
			"allowed_updates": []string{"message", "callback_query"},
		})
		if callErr != nil {
			return latestOffset, dropped, fmt.Errorf("telegram getUpdates backlog flush failed: %w", r.redactError(callErr))
		}

		var updates []struct {
			ID int64 `json:"update_id"`
		}
		if err := json.Unmarshal(raw, &updates); err != nil {
			return latestOffset, dropped, fmt.Errorf("decode getUpdates backlog flush payload: %w", err)
		}
		if len(updates) == 0 {
			return latestOffset, dropped, nil
		}
		for _, update := range updates {
			if update.ID > int64(latestOffset) {
				latestOffset = int(update.ID)
				dropped++
			}
		}
		if len(updates) < startupFlushBatchLimit {
			return latestOffset, dropped, nil
		}
	}
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

	if update.CallbackQuery != nil {
		r.handleCallbackQuery(ctx, b, update.CallbackQuery)
		return
	}

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
	text, imageFileIDs := extractMessageInput(update.Message)
	voiceFileID := collectVoiceFileID(update.Message)
	if text == "" && voiceFileID == "" {
		r.logf("message ignored: empty text/media chat=%d user=%d", chatID, userID)
		return
	}
	cmd := parseCommand(text)
	if !r.authorize(ctx, b, chatID, userID, cmd) {
		return
	}
	r.logf("authorization accepted: chat=%d user=%d command=%q", chatID, userID, cmd)
	r.logf("message received: chat=%d user=%d chars=%d images=%d voice=%t preview=%q", chatID, userID, len(text), len(imageFileIDs), voiceFileID != "", r.previewForLog(text))
	if cmd != "" {
		r.logf("command dispatch: chat=%d cmd=%s raw=%q", chatID, cmd, r.previewForLog(text))
		r.handleCommand(ctx, b, chatID, text)
		return
	}

	sessionKey := strconv.FormatInt(chatID, 10)
	queued := r.enqueuePendingTurn(sessionKey, pendingTurn{
		chatID:       chatID,
		userID:       userID,
		sessionKey:   sessionKey,
		text:         text,
		imageFileIDs: append([]string(nil), imageFileIDs...),
		voiceFileID:  voiceFileID,
		bot:          b,
	})
	r.logf("agent turn queued: chat=%d session_key=%s generation=%d messages=%d ready_at=%s", chatID, sessionKey, queued.generation, queued.messageCount, queued.readyAt.UTC().Format(time.RFC3339Nano))
	r.schedulePendingTurnDrain(sessionKey, queued.generation)
}

func (r *Runner) executeTurn(ctx context.Context, turnCtx context.Context, b *bot.Bot, chatID, userID int64, sessionKey, text string, imageFileIDs []string, voiceFileID string, opts turnExecutionOptions) {
	opts.TelegramVoiceInput = strings.TrimSpace(voiceFileID) != ""
	if opts.LocalCleanup != nil {
		defer opts.LocalCleanup()
	}
	if r.turnExecutor != nil {
		r.turnExecutor(ctx, turnCtx, b, chatID, userID, sessionKey, text, imageFileIDs)
		return
	}
	if opts.isLocalText() && strings.TrimSpace(text) != "" {
		r.logf("local text input echo: chat=%d session_key=%s source=%s chars=%d", chatID, sessionKey, strings.TrimSpace(opts.LocalSource), len(strings.TrimSpace(text)))
		r.sendChunked(ctx, b, chatID, nil, "local input:\n"+strings.TrimSpace(text))
	}
	progressText := "Working..."
	if strings.TrimSpace(voiceFileID) != "" || strings.TrimSpace(opts.LocalAudio) != "" {
		progressText = "Transcribing..."
	} else {
		thinkValue, _ := r.Engine.SessionThinkValue(ctx, "telegram", sessionKey)
		if thinkValue != "off" {
			progressText = "Thinking..."
		}
	}
	progress, _ := b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: progressText})
	if progress != nil {
		r.logf("progress message sent: chat=%d message_id=%d", chatID, progress.ID)
	}
	r.logf("agent turn start: chat=%d session_key=%s", chatID, sessionKey)
	startedAt := time.Now()
	showTools, _ := r.Engine.IsSessionShowTools(ctx, "telegram", sessionKey)
	if showTools {
		r.logf("live tool stream enabled: chat=%d session_key=%s", chatID, sessionKey)
	}
	inputImages := []string{}
	if strings.TrimSpace(opts.LocalAudio) != "" {
		transcript, transcribeErr := r.voiceService().Transcribe(turnCtx, opts.LocalAudio)
		if transcribeErr != nil {
			r.logf("local voice transcription failed: chat=%d session_key=%s source=%s error=%v", chatID, sessionKey, strings.TrimSpace(opts.LocalSource), r.redactError(transcribeErr))
			r.replyError(ctx, b, chatID, progress, fmt.Errorf("failed to transcribe local audio: %w", transcribeErr))
			return
		}
		transcript = stripLocalTranscriptArtifact(strings.TrimSpace(transcript))
		if transcript == "" {
			r.replyError(ctx, b, chatID, progress, errors.New("local audio transcription was empty"))
			return
		}
		text = transcript
		r.logf("local voice transcription success: chat=%d session_key=%s source=%s chars=%d preview=%q", chatID, sessionKey, strings.TrimSpace(opts.LocalSource), len(transcript), r.previewForLog(transcript))
		r.sendChunked(ctx, b, chatID, nil, "transcript:\n"+transcript)
	}
	if strings.TrimSpace(voiceFileID) != "" {
		voicePath, cleanup, voiceErr := fetchTelegramVoice(turnCtx, b, voiceFileID)
		if cleanup != nil {
			defer cleanup()
		}
		if voiceErr != nil {
			r.logf("voice fetch failed: chat=%d session_key=%s error=%v", chatID, sessionKey, r.redactError(voiceErr))
			r.replyError(ctx, b, chatID, progress, fmt.Errorf("failed to fetch voice attachment: %w", voiceErr))
			return
		}
		r.logf("voice fetch success: chat=%d session_key=%s path=%s", chatID, sessionKey, voicePath)
		transcript, transcribeErr := r.voiceService().Transcribe(turnCtx, voicePath)
		if transcribeErr != nil {
			r.logf("voice transcription failed: chat=%d session_key=%s error=%v", chatID, sessionKey, r.redactError(transcribeErr))
			r.replyError(ctx, b, chatID, progress, fmt.Errorf("failed to transcribe voice note: %w", transcribeErr))
			return
		}
		transcript = strings.TrimSpace(transcript)
		if transcript == "" {
			r.replyError(ctx, b, chatID, progress, errors.New("voice note transcription was empty"))
			return
		}
		if strings.TrimSpace(text) != "" {
			text = strings.TrimSpace(transcript) + "\n\nTelegram caption:\n" + strings.TrimSpace(text)
		} else {
			text = transcript
		}
		r.logf("voice transcription success: chat=%d session_key=%s chars=%d preview=%q", chatID, sessionKey, len(transcript), r.previewForLog(transcript))
		r.sendChunked(ctx, b, chatID, nil, "transcript:\n"+transcript)
	}
	if len(imageFileIDs) > 0 {
		images, imgErr := fetchTelegramImages(turnCtx, b, imageFileIDs)
		if imgErr != nil {
			r.logf("image fetch failed: chat=%d session_key=%s error=%v", chatID, sessionKey, r.redactError(imgErr))
			r.replyError(ctx, b, chatID, progress, fmt.Errorf("failed to fetch image attachment: %w", imgErr))
			return
		}
		inputImages = images
		r.logf("image fetch success: chat=%d session_key=%s images=%d", chatID, sessionKey, len(inputImages))
	}
	res, err := r.Engine.HandleTextWithOptions(turnCtx, "telegram", sessionKey, text, agent.HandleOptions{
		InputImages: inputImages,
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
	if res.Compacted {
		thresholdTokens := int(float64(r.Cfg.ContextWindowTokens) * r.Cfg.CompactionThreshold)
		compaction := r.readCompactionSnapshot(context.Background(), sessionKey, res.Session)
		notice := formatCompactionNotice(res.PromptTokens, thresholdTokens, r.Cfg.KeepRecentTurns, compaction)
		r.logf("compaction notice: chat=%d %s", chatID, r.previewForLog(notice))
		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, sendErr := b.SendMessage(sendCtx, &bot.SendMessageParams{ChatID: chatID, Text: notice})
		cancel()
		if sendErr != nil {
			r.logf("compaction notice send failed: chat=%d error=%v", chatID, r.redactError(sendErr))
		}
	}
	for i, tr := range res.ToolTrace {
		line := fmt.Sprintf("tool trace [%d/%d]: chat=%d name=%s duration_ms=%d args=%q", i+1, len(res.ToolTrace), chatID, tr.Name, tr.DurationMs, r.previewForLog(tr.ArgsJSON))
		if strings.TrimSpace(tr.Error) != "" {
			line += fmt.Sprintf(" error=%q", r.previewForLog(tr.Error))
		} else {
			line += fmt.Sprintf(" result=%q", r.previewForLog(tr.ResultJSON))
		}
		r.logf("%s", line)
	}
	displayContent := strings.TrimSpace(res.AssistantContent)
	if displayContent == "" {
		displayContent = "(empty response)"
	}
	speechContent := displayContent

	verbose, _ := r.Engine.IsSessionVerbose(ctx, "telegram", sessionKey)
	if verbose {
		sections := make([]string, 0, 2)
		if len(res.ThinkingTrace) > 0 {
			sections = append(sections, formatThinkingTrace(res.ThinkingTrace))
		}
		if len(res.ToolTrace) > 0 {
			sections = append(sections, formatToolTrace(res.ToolTrace))
		}
		if len(sections) > 0 {
			trace := strings.Join(sections, "\n\n")
			if displayContent == "(empty response)" {
				displayContent = trace
			} else {
				displayContent += "\n\n" + trace
			}
		}
	}
	r.logf("response send: chat=%d chars=%d voice_chars=%d", chatID, len(displayContent), len(speechContent))
	r.sendAssistantResponse(ctx, turnCtx, b, chatID, sessionKey, progress, displayContent, speechContent, showTools, r.responseDeliveryForTurn(ctx, sessionKey, opts))
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

func extractMessageInput(msg *models.Message) (string, []string) {
	if msg == nil {
		return "", nil
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		text = strings.TrimSpace(msg.Caption)
	}
	imageFileIDs := collectImageFileIDs(msg)
	if text == "" && len(imageFileIDs) > 0 {
		text = "Please analyze this image."
	}
	return text, imageFileIDs
}

func collectImageFileIDs(msg *models.Message) []string {
	if msg == nil {
		return nil
	}
	out := make([]string, 0, 2)
	seen := map[string]struct{}{}

	add := func(fileID string) {
		fileID = strings.TrimSpace(fileID)
		if fileID == "" {
			return
		}
		if _, ok := seen[fileID]; ok {
			return
		}
		seen[fileID] = struct{}{}
		out = append(out, fileID)
	}

	if len(msg.Photo) > 0 {
		best := msg.Photo[0]
		for _, p := range msg.Photo[1:] {
			if p.FileSize > best.FileSize {
				best = p
				continue
			}
			if p.FileSize == best.FileSize && (p.Width*p.Height) > (best.Width*best.Height) {
				best = p
			}
		}
		add(best.FileID)
	}
	if msg.Document != nil && strings.HasPrefix(strings.ToLower(strings.TrimSpace(msg.Document.MimeType)), "image/") {
		add(msg.Document.FileID)
	}

	if len(out) > maxTelegramInputImages {
		out = out[:maxTelegramInputImages]
	}
	return out
}

func collectVoiceFileID(msg *models.Message) string {
	if msg == nil || msg.Voice == nil {
		return ""
	}
	return strings.TrimSpace(msg.Voice.FileID)
}

func fetchTelegramImages(ctx context.Context, api telegramFileClient, fileIDs []string) ([]string, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		fileID = strings.TrimSpace(fileID)
		if fileID == "" {
			continue
		}
		body, _, err := downloadTelegramFile(ctx, api, fileID, telegramImageMaxBytes, telegramImageFetchTimeout)
		if err != nil {
			return nil, err
		}
		out = append(out, base64.StdEncoding.EncodeToString(body))
	}
	if len(out) == 0 {
		return nil, errors.New("no images fetched")
	}
	return out, nil
}

func fetchTelegramVoice(ctx context.Context, api telegramFileClient, fileID string) (string, func(), error) {
	body, file, err := downloadTelegramFile(ctx, api, fileID, telegramVoiceMaxBytes, telegramVoiceFetchTimeout)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "ollamaclaw-telegram-voice-*")
	if err != nil {
		return "", nil, fmt.Errorf("create voice temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(file.FilePath)))
	if ext == "" || len(ext) > 8 {
		ext = ".ogg"
	}
	path := filepath.Join(dir, "voice"+ext)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write voice file: %w", err)
	}
	return path, cleanup, nil
}

func downloadTelegramFile(ctx context.Context, api telegramFileClient, fileID string, maxBytes int64, timeout time.Duration) ([]byte, *models.File, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, nil, errors.New("empty telegram file id")
	}
	file, err := api.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, nil, fmt.Errorf("getFile(%s): %w", fileID, err)
	}
	if file == nil || strings.TrimSpace(file.FilePath) == "" {
		return nil, nil, fmt.Errorf("getFile(%s): empty file path", fileID)
	}
	link := strings.TrimSpace(api.FileDownloadLink(file))
	if link == "" {
		return nil, nil, fmt.Errorf("getFile(%s): empty download link", fileID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build download request for %s: %w", fileID, err)
	}
	httpClient := &http.Client{Timeout: timeout}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("download file %s: %w", fileID, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	_ = res.Body.Close()
	if readErr != nil {
		return nil, nil, fmt.Errorf("read file %s: %w", fileID, readErr)
	}
	if res.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("download file %s status %d", fileID, res.StatusCode)
	}
	if len(body) == 0 {
		return nil, nil, fmt.Errorf("download file %s: empty body", fileID)
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, fmt.Errorf("download file %s too large (%d bytes > %d)", fileID, len(body), maxBytes)
	}
	return body, file, nil
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
		r.resyncCommandsBestEffort("start")
		send("OllamaClaw is ready.\nCommands:\n/start\n/help\n/model [name]\n/tools\n/reminder list [active|all]\n/reminder safe <id>\n/reminder unsafe <id>\n/reminder prefetch list <id>\n/agents list [active|all]\n/agents show <id>\n/agents cancel <id>\n/show tools [on|off]\n/show thinking [on|off]\n/show dreaming [on|off]\n/verbose [on|off]\n/think [on|off|low|medium|high|xhigh|default]\n/voice [off|text|audio|both]\n/voice output [mac|telegram|both]\n/dream\n/status\n/fullsystem\n/reset\n/stop\n/restart")
	case "help":
		r.logf("command help: chat=%d", chatID)
		send("Commands:\n/start\n/help\n/model [name]\n/tools\n/reminder list [active|all]\n/reminder safe <id>\n/reminder unsafe <id>\n/reminder prefetch list <id>\n/agents list [active|all]\n/agents show <id>\n/agents cancel <id>\n/show tools [on|off]\n/show thinking [on|off]\n/show dreaming [on|off]\n/verbose [on|off]\n/think [on|off|low|medium|high|xhigh|default]\n/voice [off|text|audio|both]\n/voice output [mac|telegram|both]\n/dream\n/status\n/fullsystem\n/reset\n/stop\n/restart\n\nSend text, photos, or voice notes to chat with OllamaClaw.")
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
			lines = append(lines, "- "+t.Name)
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
			send("usage: /show <tools|thinking|dreaming> [on|off]")
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
		case "dreaming", "dream", "memory", "memories":
			if len(parts) == 2 {
				if err := r.Engine.SetSessionDreamingNotifications(ctx, "telegram", sessionKey, true); err != nil {
					r.logf("command show dreaming set failed: chat=%d error=%v", chatID, r.redactError(err))
					sendErr(err)
					return
				}
				send("dreaming notifications: true")
				return
			}
			enabled, ok := parseOnOff(parts[2])
			if !ok {
				send("usage: /show dreaming [on|off]")
				return
			}
			if err := r.Engine.SetSessionDreamingNotifications(ctx, "telegram", sessionKey, enabled); err != nil {
				r.logf("command show dreaming set failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			r.logf("command show dreaming set: chat=%d enabled=%t", chatID, enabled)
			send(fmt.Sprintf("dreaming notifications: %t", enabled))
		default:
			send("usage: /show <tools|thinking|dreaming> [on|off]")
		}
	case "think":
		r.logf("command think: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if len(parts) == 1 {
			value, err := r.Engine.SessionThinkValue(ctx, "telegram", sessionKey)
			if err != nil {
				r.logf("command think read failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			send(fmt.Sprintf("think: %s", value))
			return
		}
		value, ok := parseThinkValue(parts[1])
		if !ok {
			send("usage: /think [on|off|low|medium|high|xhigh|default]")
			return
		}
		if err := r.Engine.SetSessionThinkValue(ctx, "telegram", sessionKey, value); err != nil {
			r.logf("command think set failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		r.logf("command think set: chat=%d value=%s", chatID, value)
		send(fmt.Sprintf("think: %s", value))
	case "voice":
		r.logf("command voice: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if len(parts) == 1 || strings.EqualFold(strings.TrimSpace(parts[1]), "status") {
			mode, err := r.sessionVoiceReplyMode(ctx, sessionKey)
			if err != nil {
				r.logf("command voice read failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			outputMode, err := r.sessionVoiceOutputMode(ctx, sessionKey)
			if err != nil {
				r.logf("command voice output read failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			send(fmt.Sprintf("voice reply: %s\nvoice output: %s\nstt_model: %s\nkokoro_voice: %s", mode, outputMode, r.Cfg.Voice.TranscriptionModel, r.Cfg.Voice.KokoroVoice))
			return
		}
		if strings.EqualFold(strings.TrimSpace(parts[1]), "output") {
			if len(parts) == 2 {
				mode, err := r.sessionVoiceOutputMode(ctx, sessionKey)
				if err != nil {
					r.logf("command voice output read failed: chat=%d error=%v", chatID, r.redactError(err))
					sendErr(err)
					return
				}
				send("voice output: " + mode)
				return
			}
			mode, ok := normalizeVoiceOutputMode(parts[2])
			if !ok {
				send("usage: /voice output [mac|telegram|both]")
				return
			}
			if err := r.setSessionVoiceOutputMode(ctx, sessionKey, mode); err != nil {
				r.logf("command voice output set failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			r.logf("command voice output set: chat=%d mode=%s", chatID, mode)
			send("voice output: " + mode)
			return
		}
		valueArg := strings.TrimSpace(parts[1])
		if strings.EqualFold(valueArg, "reply") && len(parts) >= 3 {
			valueArg = strings.TrimSpace(parts[2])
		}
		mode, ok := normalizeVoiceReplyMode(valueArg)
		if !ok {
			send("usage: /voice [off|text|audio|both]\nusage: /voice output [mac|telegram|both]")
			return
		}
		if err := r.setSessionVoiceReplyMode(ctx, sessionKey, mode); err != nil {
			r.logf("command voice set failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		r.logf("command voice set: chat=%d mode=%s", chatID, mode)
		send("voice reply: " + mode)
	case "dream":
		r.logf("command dream: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		started, err := r.Engine.TriggerCoreMemoriesRefresh(ctx, "telegram", sessionKey)
		if err != nil {
			r.logf("command dream failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		if !started {
			send("dreaming is already in progress for this session")
			return
		}
		send("dreaming refresh triggered")
	case "reminder":
		r.logf("command reminder: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if r.Scheduler == nil {
			send("reminder scheduler is unavailable")
			return
		}
		if len(parts) < 2 {
			send("usage: /reminder <list|safe|unsafe|prefetch>")
			return
		}
		action := strings.ToLower(strings.TrimSpace(parts[1]))
		switch action {
		case "list":
			activeOnly := true
			if len(parts) >= 3 {
				scope := strings.ToLower(strings.TrimSpace(parts[2]))
				switch scope {
				case "all":
					activeOnly = false
				case "active", "":
					activeOnly = true
				default:
					send("usage: /reminder list [active|all]")
					return
				}
			}
			jobs, err := r.Scheduler.ListReminders(ctx, activeOnly)
			if err != nil {
				r.logf("command reminder list failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			scopeLabel := "active"
			if !activeOnly {
				scopeLabel = "all"
			}
			if len(jobs) == 0 {
				send(fmt.Sprintf("reminders (%s): none", scopeLabel))
				return
			}
			lines := make([]string, 0, len(jobs)+1)
			lines = append(lines, fmt.Sprintf("reminders (%s, timezone=%s):", scopeLabel, util.PacificTimezoneName))
			for _, job := range jobs {
				nextRun := "-"
				if strings.TrimSpace(job.NextRunAt) != "" {
					nextRun = job.NextRunAt
				}
				lastErr := strings.TrimSpace(job.LastError)
				if lastErr == "" {
					lastErr = "-"
				} else {
					lastErr = truncateForLive(lastErr)
				}
				lines = append(lines, fmt.Sprintf("- %s mode=%s safe=%t auto_prefetch=%t active=%t schedule=%q next=%s err=%s", job.ID, job.Mode, job.Safe, job.AutoPrefetch, job.Active, job.CompiledSchedule, nextRun, lastErr))
			}
			r.sendChunked(ctx, b, chatID, nil, strings.Join(lines, "\n"))
		case "safe", "unsafe":
			if len(parts) < 3 {
				send(fmt.Sprintf("usage: /reminder %s <id>", action))
				return
			}
			id := strings.TrimSpace(parts[2])
			if id == "" {
				send(fmt.Sprintf("usage: /reminder %s <id>", action))
				return
			}
			safe := action == "safe"
			info, err := r.Scheduler.SetReminderSafe(ctx, id, safe)
			if err != nil {
				r.logf("command reminder %s failed: chat=%d id=%s error=%v", action, chatID, id, r.redactError(err))
				sendErr(err)
				return
			}
			r.logf("command reminder %s set: chat=%d id=%s safe=%t", action, chatID, id, safe)
			send(fmt.Sprintf("reminder %s: %s (safe=%t)", action, info.ID, info.Safe))
		case "prefetch":
			if len(parts) < 4 {
				send("usage: /reminder prefetch list <id>")
				return
			}
			prefetchAction := strings.ToLower(strings.TrimSpace(parts[2]))
			if prefetchAction != "list" {
				send("usage: /reminder prefetch list <id>")
				return
			}
			id := strings.TrimSpace(parts[3])
			if id == "" {
				send("usage: /reminder prefetch list <id>")
				return
			}
			commands, err := r.Scheduler.ListReminderPrefetchCommands(ctx, id)
			if err != nil {
				r.logf("command reminder prefetch list failed: chat=%d id=%s error=%v", chatID, id, r.redactError(err))
				sendErr(err)
				return
			}
			if len(commands) == 0 {
				send(fmt.Sprintf("reminder prefetch %s: none", id))
				return
			}
			lines := make([]string, 0, len(commands)+1)
			lines = append(lines, fmt.Sprintf("reminder prefetch %s:", id))
			for _, command := range commands {
				lines = append(lines, "- "+command)
			}
			r.sendChunked(ctx, b, chatID, nil, strings.Join(lines, "\n"))
		default:
			send("usage: /reminder <list|safe|unsafe|prefetch>")
		}
	case "agents":
		r.logf("command agents: chat=%d args=%q", chatID, r.previewForLog(strings.Join(parts[1:], " ")))
		if r.Subagents == nil {
			send("subagent manager is unavailable")
			return
		}
		action := "list"
		if len(parts) >= 2 {
			action = strings.ToLower(strings.TrimSpace(parts[1]))
		}
		switch action {
		case "list":
			scope := "active"
			if len(parts) >= 3 {
				scope = strings.ToLower(strings.TrimSpace(parts[2]))
			}
			if scope != "active" && scope != "all" {
				send("usage: /agents list [active|all]")
				return
			}
			tasks, err := r.Subagents.ListSubagentTasks(ctx, tools.SubagentTaskFilter{Limit: 50})
			if err != nil {
				r.logf("command agents list failed: chat=%d error=%v", chatID, r.redactError(err))
				sendErr(err)
				return
			}
			if scope == "active" {
				filtered := tasks[:0]
				for _, task := range tasks {
					if task.Status == "queued" || task.Status == "running" {
						filtered = append(filtered, task)
					}
				}
				tasks = filtered
			}
			r.sendChunked(ctx, b, chatID, nil, formatSubagentList(tasks, scope))
		case "show", "status":
			if len(parts) < 3 {
				send("usage: /agents show <id>")
				return
			}
			id := strings.TrimSpace(parts[2])
			result, ok, err := r.Subagents.GetSubagentResult(ctx, id)
			if err != nil {
				r.logf("command agents show failed: chat=%d id=%s error=%v", chatID, id, r.redactError(err))
				sendErr(err)
				return
			}
			if !ok {
				send("subagent task not found: " + id)
				return
			}
			r.sendChunked(ctx, b, chatID, nil, formatSubagentResult(result))
		case "cancel", "stop":
			if len(parts) < 3 {
				send("usage: /agents cancel <id>")
				return
			}
			id := strings.TrimSpace(parts[2])
			info, err := r.Subagents.CancelSubagentTask(ctx, id)
			if err != nil {
				r.logf("command agents cancel failed: chat=%d id=%s error=%v", chatID, id, r.redactError(err))
				sendErr(err)
				return
			}
			send(fmt.Sprintf("subagent canceled:\nid: %s\nstatus: %s", info.ID, info.Status))
		default:
			send("usage: /agents <list|show|cancel>")
		}
	case "status":
		r.logf("command status: chat=%d", chatID)
		verbose, _ := r.Engine.IsSessionVerbose(ctx, "telegram", sessionKey)
		showTools, _ := r.Engine.IsSessionShowTools(ctx, "telegram", sessionKey)
		thinkValue, _ := r.Engine.SessionThinkValue(ctx, "telegram", sessionKey)
		voiceMode, _ := r.sessionVoiceReplyMode(ctx, sessionKey)
		voiceOutput, _ := r.sessionVoiceOutputMode(ctx, sessionKey)
		dreamingNotifications, _ := r.Engine.IsSessionDreamingNotifications(ctx, "telegram", sessionKey)
		estimate, estErr := r.Engine.EstimateNextPrompt(ctx, "telegram", sessionKey)
		if estErr != nil {
			r.logf("command status prompt estimate failed: chat=%d error=%v", chatID, r.redactError(estErr))
		}
		version := strings.TrimSpace(r.AppVersion)
		if version == "" {
			version = "dev"
		}
		thresholdTokens := int(float64(r.Cfg.ContextWindowTokens) * r.Cfg.CompactionThreshold)
		compaction := r.readCompactionSnapshot(ctx, sessionKey, sess)
		webhookRepos := "-"
		if len(r.Cfg.GitHubWebhook.RepoAllowlist) > 0 {
			webhookRepos = strings.Join(r.Cfg.GitHubWebhook.RepoAllowlist, ", ")
		}
		text := fmt.Sprintf("status:\nversion: %s\nmodel: %s\nverbose: %t\nshow_tools: %t\nthink: %s\nvoice_reply: %s\nvoice_output: %s\ndreaming_notifications: %t\ntimezone: %s\nnext_prompt_tokens_est: %d\nnext_prompt_chars_est: %d\nnext_prompt_messages: %d\nnext_prompt_tools: %d\nprompt_estimator: %s\nlifetime_prompt_tokens: %d\nlifetime_completion_tokens: %d\ncompactions: %d\ncontext_window_tokens: %d\ncompaction_threshold: %.2f\ncompaction_trigger_tokens: %d\nkeep_recent_turns: %d\nlast_compaction_at: %s\nlast_compaction_summary_chars: %d\nlast_compaction_archived_before_seq: %d\ngithub_webhook_enabled: %t\ngithub_webhook_listen_addr: %s\ngithub_webhook_owner_login: %s\ngithub_webhook_repo_allowlist: %s\ndb: %s\nlog: %s", version, redactTelegramToken(r.Cfg.Telegram.BotToken, sess.ModelOverride), verbose, showTools, thinkValue, voiceMode, voiceOutput, dreamingNotifications, util.PacificTimezoneName, estimate.EstimatedTokens, estimate.RequestChars, estimate.MessageCount, estimate.ToolCount, estimate.EstimatorFormula, sess.TotalPromptToken, sess.TotalEvalToken, compaction.TotalCount, r.Cfg.ContextWindowTokens, r.Cfg.CompactionThreshold, thresholdTokens, r.Cfg.KeepRecentTurns, compaction.LastAt, compaction.SummaryChars, compaction.ArchivedBeforeSeq, githubWebhookEnabled(r.Cfg), strings.TrimSpace(r.Cfg.GitHubWebhook.ListenAddr), strings.TrimSpace(r.Cfg.GitHubWebhook.OwnerLogin), webhookRepos, r.Cfg.DBPath, strings.TrimSpace(r.Cfg.LogPath))
		send(text)
	case "fullsystem":
		r.logf("command fullsystem: chat=%d", chatID)
		full, err := r.Engine.FullSystemContext(ctx, "telegram", sessionKey)
		if err != nil {
			r.logf("command fullsystem failed: chat=%d error=%v", chatID, r.redactError(err))
			sendErr(err)
			return
		}
		if strings.TrimSpace(full) == "" {
			full = "(system context unavailable)"
		}
		r.sendChunked(ctx, b, chatID, nil, full)
	case "stop":
		r.logf("command stop: chat=%d", chatID)
		turn, ok := r.stopTurn(sessionKey)
		if !ok {
			send("no active turn to stop")
			return
		}
		r.logf("command stop signaled: chat=%d session_key=%s turn_id=%d", chatID, sessionKey, turn.id)
		send("stopping current turn...")
	case "restart":
		r.logf("command restart: chat=%d", chatID)
		turn, ok := r.stopTurn(sessionKey)
		if ok {
			r.logf("command restart interrupted turn: chat=%d session_key=%s turn_id=%d", chatID, sessionKey, turn.id)
		}
		if !r.requestRestart() {
			send("restart unavailable right now")
			return
		}
		msg := "restarting now..."
		if ok {
			msg = "restarting now (active turn interrupted)..."
		}
		sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = b.SendMessage(sendCtx, &bot.SendMessageParams{ChatID: chatID, Text: msg})
	default:
		r.logf("unknown command: chat=%d cmd=%s", chatID, cmd)
		send("unknown command")
	}
}

func (r *Runner) setRunCancel(cancel context.CancelFunc) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	r.runCancel = cancel
}

func (r *Runner) requestRestart() bool {
	r.runMu.Lock()
	cancel := r.runCancel
	r.runMu.Unlock()
	if cancel == nil {
		return false
	}
	r.restarting.Store(true)
	cancel()
	return true
}

func (r *Runner) resyncCommandsBestEffort(reason string) {
	token := strings.TrimSpace(r.Cfg.Telegram.BotToken)
	if token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := SyncCommands(ctx, token); err != nil {
		r.logf("telegram command resync (%s) failed: %v", reason, r.redactError(err))
		return
	}
	r.logf("telegram commands resynced (%s)", reason)
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

func (r *Runner) enqueuePendingTurn(sessionKey string, turn pendingTurn) pendingTurn {
	turn.sessionKey = sessionKey
	turn.text = strings.TrimSpace(turn.text)
	turn.imageFileIDs = append([]string(nil), turn.imageFileIDs...)
	turn.voiceFileID = strings.TrimSpace(turn.voiceFileID)
	turn.localSource = strings.TrimSpace(turn.localSource)
	turn.localAudio = strings.TrimSpace(turn.localAudio)
	if turn.messageCount <= 0 {
		turn.messageCount = 1
	}
	r.pendingMu.Lock()
	if r.pendingTurns == nil {
		r.pendingTurns = map[string]pendingTurn{}
	}
	now := time.Now()
	if existing, ok := r.pendingTurns[sessionKey]; ok {
		existing.text = joinPendingText(existing.text, turn.text)
		existing.imageFileIDs = mergePendingImageIDs(existing.imageFileIDs, turn.imageFileIDs)
		if turn.voiceFileID != "" {
			existing.voiceFileID = turn.voiceFileID
		}
		if turn.localSource != "" {
			existing.localSource = turn.localSource
		}
		if turn.localAudio != "" {
			if existing.localCleanup != nil && existing.localAudio != "" && existing.localAudio != turn.localAudio {
				existing.localCleanup()
			}
			existing.localAudio = turn.localAudio
			existing.localCleanup = turn.localCleanup
		} else if turn.localCleanup != nil {
			turn.localCleanup()
		}
		existing.noDebounce = existing.noDebounce || turn.noDebounce
		if turn.chatID != 0 {
			existing.chatID = turn.chatID
		}
		if turn.userID != 0 {
			existing.userID = turn.userID
		}
		if turn.bot != nil {
			existing.bot = turn.bot
		}
		existing.messageCount += turn.messageCount
		existing.generation = r.nextPending.Add(1)
		if existing.noDebounce {
			existing.readyAt = now
		} else {
			existing.readyAt = now.Add(pendingTurnDebounce)
		}
		r.pendingTurns[sessionKey] = existing
		turn = existing
	} else {
		turn.generation = r.nextPending.Add(1)
		if turn.noDebounce {
			turn.readyAt = now
		} else {
			turn.readyAt = now.Add(pendingTurnDebounce)
		}
		r.pendingTurns[sessionKey] = turn
	}
	r.pendingMu.Unlock()
	return turn
}

func joinPendingText(existing, incoming string) string {
	existing = strings.TrimSpace(existing)
	incoming = strings.TrimSpace(incoming)
	if existing == "" {
		return incoming
	}
	if incoming == "" {
		return existing
	}
	return existing + "\n" + incoming
}

func mergePendingImageIDs(existing, incoming []string) []string {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	out := make([]string, 0, len(existing)+len(incoming))
	seen := map[string]struct{}{}
	add := func(ids []string) {
		for _, raw := range ids {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			if len(out) >= maxTelegramInputImages {
				return
			}
		}
	}
	add(existing)
	if len(out) < maxTelegramInputImages {
		add(incoming)
	}
	return out
}

func (r *Runner) schedulePendingTurnDrain(sessionKey string, generation uint64) {
	go r.drainPendingTurn(sessionKey, generation)
}

func (r *Runner) drainPendingTurn(sessionKey string, generation uint64) {
	for {
		turn, ok := r.pendingTurnByGeneration(sessionKey, generation)
		if !ok {
			return
		}
		if wait := time.Until(turn.readyAt); wait > 0 {
			time.Sleep(wait)
			continue
		}
		turnCtx, turnCancel := context.WithCancel(context.Background())
		turnCtx = tools.WithBashApprover(turnCtx, &telegramBashApprover{
			r:          r,
			bot:        turn.bot,
			chatID:     turn.chatID,
			userID:     turn.userID,
			sessionKey: turn.sessionKey,
		})
		turnID, started := r.beginTurn(turn.sessionKey, turn.chatID, turnCancel)
		if !started {
			turnCancel()
			time.Sleep(pendingTurnRetry)
			continue
		}
		if !r.consumePendingTurnByGeneration(turn.sessionKey, generation) {
			r.endTurn(turn.sessionKey, turnID)
			turnCancel()
			time.Sleep(pendingTurnRetry)
			continue
		}
		if turn.bot == nil && r.turnExecutor == nil {
			r.logf("queued turn dropped: chat=%d session_key=%s generation=%d reason=nil_bot", turn.chatID, turn.sessionKey, generation)
			r.endTurn(turn.sessionKey, turnID)
			turnCancel()
			return
		}
		r.logf("queued turn start: chat=%d session_key=%s generation=%d", turn.chatID, turn.sessionKey, generation)
		func() {
			defer turnCancel()
			defer r.endTurn(turn.sessionKey, turnID)
			r.executeTurn(context.Background(), turnCtx, turn.bot, turn.chatID, turn.userID, turn.sessionKey, turn.text, turn.imageFileIDs, turn.voiceFileID, turnExecutionOptions{
				LocalSource:  turn.localSource,
				LocalAudio:   turn.localAudio,
				LocalCleanup: turn.localCleanup,
			})
		}()
		return
	}
}

func (r *Runner) pendingTurnByGeneration(sessionKey string, generation uint64) (pendingTurn, bool) {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	turn, ok := r.pendingTurns[sessionKey]
	if !ok || turn.generation != generation {
		return pendingTurn{}, false
	}
	return turn, true
}

func (r *Runner) consumePendingTurnByGeneration(sessionKey string, generation uint64) bool {
	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	turn, ok := r.pendingTurns[sessionKey]
	if !ok || turn.generation != generation {
		return false
	}
	delete(r.pendingTurns, sessionKey)
	return true
}

type compactionSnapshot struct {
	TotalCount        int
	LastAt            string
	SummaryChars      int
	ArchivedBeforeSeq int
}

func (r *Runner) readCompactionSnapshot(ctx context.Context, sessionKey string, fallback db.Session) compactionSnapshot {
	snap := compactionSnapshot{
		TotalCount:        fallback.CompactionCount,
		LastAt:            "-",
		SummaryChars:      0,
		ArchivedBeforeSeq: 0,
	}
	if r.Store == nil {
		return snap
	}
	if sess, ok, err := r.Store.GetActiveSession(ctx, "telegram", sessionKey); err == nil && ok {
		snap.TotalCount = sess.CompactionCount
	}
	sessionID := strings.TrimSpace(fallback.ID)
	if sessionID == "" {
		return snap
	}
	if c, ok, err := r.Store.LatestCompaction(ctx, sessionID); err == nil && ok {
		if !c.CreatedAt.IsZero() {
			snap.LastAt = util.FormatPacificRFC3339(c.CreatedAt)
		}
		snap.SummaryChars = len([]rune(strings.TrimSpace(c.Summary)))
		snap.ArchivedBeforeSeq = c.ArchivedBeforeSeq
	}
	return snap
}

func formatCompactionNotice(promptTokens, thresholdTokens, keepRecentTurns int, snap compactionSnapshot) string {
	return fmt.Sprintf("context compacted:\nprompt_tokens: %d\nthreshold_tokens: %d\nkeep_recent_turns: %d\ncompactions_total: %d\nlast_compaction_at: %s", promptTokens, thresholdTokens, keepRecentTurns, snap.TotalCount, snap.LastAt)
}

func formatCoreMemoryEvent(ev agent.CoreMemoryEvent) string {
	model := strings.TrimSpace(ev.Model)
	if model == "" {
		model = "(default)"
	}
	switch ev.Phase {
	case agent.CoreMemoryEventStart:
		return fmt.Sprintf("dreaming started:\nuser_turns: %d\nmodel: %s", ev.UserTurnCount, model)
	case agent.CoreMemoryEventDone:
		status := "unchanged"
		if ev.Updated {
			status = "updated"
		}
		lines := []string{
			"dreaming done:",
			fmt.Sprintf("status: %s", status),
			fmt.Sprintf("changes: +%d -%d =%d", ev.Delta.AddedCount, ev.Delta.RemovedCount, ev.Delta.KeptCount),
			fmt.Sprintf("chars: %d -> %d", ev.Delta.BeforeChars, ev.Delta.AfterChars),
		}
		if len(ev.Delta.AddedPreview) > 0 {
			lines = append(lines, "added:")
			for _, item := range ev.Delta.AddedPreview {
				lines = append(lines, "- "+item)
			}
		}
		if len(ev.Delta.RemovedPreview) > 0 {
			lines = append(lines, "removed:")
			for _, item := range ev.Delta.RemovedPreview {
				lines = append(lines, "- "+item)
			}
		}
		lines = append(lines,
			fmt.Sprintf("duration_ms: %d", ev.DurationMs),
			fmt.Sprintf("user_turns: %d", ev.UserTurnCount),
			fmt.Sprintf("model: %s", model),
		)
		return strings.Join(lines, "\n")
	case agent.CoreMemoryEventFailure:
		errMsg := strings.TrimSpace(ev.Error)
		if errMsg == "" {
			errMsg = "unknown error"
		}
		return fmt.Sprintf("dreaming failed:\nerror: %s\nduration_ms: %d\nuser_turns: %d\nmodel: %s", truncateForLive(errMsg), ev.DurationMs, ev.UserTurnCount, model)
	default:
		return fmt.Sprintf("dreaming event:\nphase: %s\nduration_ms: %d\nuser_turns: %d\nmodel: %s", strings.TrimSpace(string(ev.Phase)), ev.DurationMs, ev.UserTurnCount, model)
	}
}

func formatSubagentList(tasks []tools.SubagentInfo, scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = "active"
	}
	if len(tasks) == 0 {
		return fmt.Sprintf("subagents (%s): none", scope)
	}
	lines := make([]string, 0, len(tasks)+1)
	lines = append(lines, fmt.Sprintf("subagents (%s):", scope))
	for _, task := range tasks {
		target := "-"
		if strings.TrimSpace(task.Repo) != "" || task.PRNumber > 0 {
			target = fmt.Sprintf("%s#%d", fallbackTelegramField(task.Repo, "-"), task.PRNumber)
		}
		when := fallbackTelegramField(task.UpdatedAt, task.CreatedAt)
		errText := strings.TrimSpace(task.Error)
		if errText != "" {
			errText = " err=" + truncateForLive(errText)
		}
		lines = append(lines, fmt.Sprintf("- %s status=%s kind=%s target=%s updated=%s%s", task.ID, task.Status, task.Kind, target, fallbackTelegramField(when, "-"), errText))
	}
	return strings.Join(lines, "\n")
}

func formatSubagentResult(result tools.SubagentResult) string {
	task := result.Info
	lines := []string{
		"subagent:",
		"id: " + task.ID,
		"status: " + task.Status,
		"kind: " + task.Kind,
	}
	if strings.TrimSpace(task.Title) != "" {
		lines = append(lines, "title: "+task.Title)
	}
	if strings.TrimSpace(task.Repo) != "" || task.PRNumber > 0 {
		lines = append(lines, fmt.Sprintf("target: %s#%d", fallbackTelegramField(task.Repo, "-"), task.PRNumber))
	}
	if task.ExitCode != nil {
		lines = append(lines, fmt.Sprintf("exit_code: %d", *task.ExitCode))
	}
	if strings.TrimSpace(task.Error) != "" {
		lines = append(lines, "error: "+task.Error)
	}
	if strings.TrimSpace(result.ResultPath) != "" {
		lines = append(lines, "result: "+result.ResultPath)
	}
	content := strings.TrimSpace(result.Content)
	if content != "" {
		lines = append(lines, "", content)
		if result.Truncated {
			lines = append(lines, "", "...[truncated]")
		}
	}
	return strings.Join(lines, "\n")
}

func fallbackTelegramField(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return strings.TrimSpace(v)
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

func (r *Runner) responseDeliveryForTurn(ctx context.Context, sessionKey string, opts turnExecutionOptions) responseDelivery {
	if opts.isLocalVoice() {
		mode, err := r.sessionVoiceOutputMode(ctx, sessionKey)
		if err != nil {
			r.logf("voice output mode read failed: session_key=%s error=%v", sessionKey, r.redactError(err))
			mode = defaultVoiceOutputMode
		}
		return responseDelivery{
			SendText:      true,
			TelegramVoice: mode == "telegram" || mode == "both",
			LocalSpeech:   mode == "mac" || mode == "both",
		}
	}
	if opts.isLocalText() {
		return responseDelivery{SendText: true}
	}
	if !opts.TelegramVoiceInput {
		return responseDelivery{SendText: true}
	}
	mode, err := r.sessionVoiceReplyMode(ctx, sessionKey)
	if err != nil {
		r.logf("voice reply mode read failed: session_key=%s error=%v", sessionKey, r.redactError(err))
		mode = defaultVoiceReplyMode
	}
	return responseDelivery{
		SendText:      mode != "audio",
		TelegramVoice: mode == "audio" || mode == "both",
	}
}

func (r *Runner) sendAssistantResponse(ctx context.Context, turnCtx context.Context, b *bot.Bot, chatID int64, sessionKey string, progress *models.Message, text, speechText string, showTools bool, delivery responseDelivery) {
	if !delivery.SendText && !delivery.TelegramVoice && !delivery.LocalSpeech {
		delivery.SendText = true
	}

	if showTools && progress != nil {
		deleted, delErr := b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: progress.ID})
		if delErr != nil || !deleted {
			r.logf("delete progress failed: chat=%d message_id=%d deleted=%t error=%v", chatID, progress.ID, deleted, r.redactError(delErr))
		} else {
			r.logf("progress message deleted: chat=%d message_id=%d", chatID, progress.ID)
		}
		progress = nil
	}

	textSent := false
	if delivery.SendText {
		r.sendChunked(ctx, b, chatID, progress, text)
		textSent = true
		progress = nil
	} else if progress != nil {
		deleted, delErr := b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: progress.ID})
		if delErr != nil || !deleted {
			r.logf("delete progress before voice-only reply failed: chat=%d message_id=%d deleted=%t error=%v", chatID, progress.ID, deleted, r.redactError(delErr))
		}
		progress = nil
	}

	if !delivery.TelegramVoice && !delivery.LocalSpeech {
		return
	}

	var voice *audio.VoiceFile
	getVoice := func() (*audio.VoiceFile, error) {
		if voice != nil {
			return voice, nil
		}
		generated, err := r.voiceService().Synthesize(turnCtx, speechText)
		if err != nil {
			return nil, err
		}
		voice = &generated
		return voice, nil
	}
	defer func() {
		if voice != nil && voice.Cleanup != nil {
			voice.Cleanup()
		}
	}()

	if delivery.TelegramVoice {
		generated, err := getVoice()
		if err == nil {
			err = r.sendVoiceFile(ctx, b, chatID, *generated)
		}
		if err != nil {
			r.logf("voice reply failed: chat=%d session_key=%s error=%v", chatID, sessionKey, r.redactError(err))
			if !textSent {
				r.sendChunked(ctx, b, chatID, nil, text)
				textSent = true
			} else {
				sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = b.SendMessage(sendCtx, &bot.SendMessageParams{ChatID: chatID, Text: "voice reply error: " + r.redactError(err).Error()})
			}
		}
	}

	if !delivery.LocalSpeech {
		return
	}
	generated, err := getVoice()
	if err == nil {
		err = r.voiceService().PlayWAV(turnCtx, generated.WAVPath)
	}
	if err != nil {
		r.logf("voice reply failed: chat=%d session_key=%s error=%v", chatID, sessionKey, r.redactError(err))
		if !textSent {
			r.sendChunked(ctx, b, chatID, nil, text)
		} else {
			sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = b.SendMessage(sendCtx, &bot.SendMessageParams{ChatID: chatID, Text: "local voice playback error: " + r.redactError(err).Error()})
		}
		return
	}
	r.logf("local voice reply played: chat=%d chars=%d", chatID, len(generated.SpeechText))
}

func (r *Runner) sendVoiceReply(ctx context.Context, b *bot.Bot, chatID int64, text string) error {
	voice, err := r.voiceService().Synthesize(ctx, text)
	if err != nil {
		return err
	}
	defer voice.Cleanup()
	return r.sendVoiceFile(ctx, b, chatID, voice)
}

func (r *Runner) sendVoiceFile(ctx context.Context, b *bot.Bot, chatID int64, voice audio.VoiceFile) error {
	if b == nil {
		return errors.New("telegram bot is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = b.SendChatAction(sendCtx, &bot.SendChatActionParams{ChatID: chatID, Action: models.ChatActionUploadVoice})
	cancel()

	f, err := os.Open(voice.Path)
	if err != nil {
		return fmt.Errorf("open generated voice: %w", err)
	}
	defer f.Close()

	sendCtx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = b.SendVoice(sendCtx, &bot.SendVoiceParams{
		ChatID:   chatID,
		Voice:    &models.InputFileUpload{Filename: "ollamaclaw.ogg", Data: f},
		Duration: voice.DurationSeconds,
	})
	if err != nil {
		return fmt.Errorf("send telegram voice: %w", err)
	}
	r.logf("voice reply sent: chat=%d duration=%d chars=%d", chatID, voice.DurationSeconds, len(voice.SpeechText))
	return nil
}

func (r *Runner) voiceService() *audio.Service {
	r.voiceMu.Lock()
	defer r.voiceMu.Unlock()
	if r.voice == nil {
		r.voice = audio.NewServiceFromConfig(r.Cfg)
	}
	return r.voice
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

func stripLocalTranscriptArtifact(transcript string) string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return ""
	}
	fields := strings.Fields(transcript)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	normalized := strings.Trim(strings.ToLower(last), `"'“”‘’.,!?;:()[]{}<>`)
	if normalized != "false" {
		return transcript
	}
	if len(fields) == 1 {
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(transcript, last))
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

func parseThinkValue(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "1", "true", "yes":
		return "on", true
	case "off", "0", "false", "no":
		return "off", true
	case "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(raw)), true
	case "default", "auto":
		return "default", true
	default:
		return "", false
	}
}

func normalizeVoiceReplyMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "on", "yes", "both":
		return "both", true
	case "0", "false", "off", "no", "none":
		return "off", true
	case "text":
		return "text", true
	case "audio", "voice":
		return "audio", true
	default:
		return "", false
	}
}

func normalizeVoiceOutputMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "mac", "local", "speaker", "speakers":
		return "mac", true
	case "telegram", "tg", "voice":
		return "telegram", true
	case "both", "all":
		return "both", true
	default:
		return "", false
	}
}

func (r *Runner) sessionVoiceReplyMode(ctx context.Context, sessionKey string) (string, error) {
	if r.Store == nil {
		return defaultVoiceReplyMode, nil
	}
	v, ok, err := r.Store.GetSetting(ctx, telegramVoiceReplyModeSettingKey(sessionKey))
	if err != nil {
		return "", err
	}
	if !ok {
		return defaultVoiceReplyMode, nil
	}
	mode, valid := normalizeVoiceReplyMode(v)
	if !valid {
		return defaultVoiceReplyMode, nil
	}
	return mode, nil
}

func (r *Runner) setSessionVoiceReplyMode(ctx context.Context, sessionKey, mode string) error {
	normalized, ok := normalizeVoiceReplyMode(mode)
	if !ok {
		return fmt.Errorf("invalid voice reply mode: %q", mode)
	}
	if r.Store == nil {
		return errors.New("settings store is unavailable")
	}
	return r.Store.SetSetting(ctx, telegramVoiceReplyModeSettingKey(sessionKey), normalized)
}

func telegramVoiceReplyModeSettingKey(sessionKey string) string {
	return settingTelegramVoiceReplyModeKey + ":" + strings.TrimSpace(sessionKey)
}

func (r *Runner) sessionVoiceOutputMode(ctx context.Context, sessionKey string) (string, error) {
	if r.Store == nil {
		return defaultVoiceOutputMode, nil
	}
	v, ok, err := r.Store.GetSetting(ctx, telegramVoiceOutputModeSettingKey(sessionKey))
	if err != nil {
		return "", err
	}
	if !ok {
		return defaultVoiceOutputMode, nil
	}
	mode, valid := normalizeVoiceOutputMode(v)
	if !valid {
		return defaultVoiceOutputMode, nil
	}
	return mode, nil
}

func (r *Runner) setSessionVoiceOutputMode(ctx context.Context, sessionKey, mode string) error {
	normalized, ok := normalizeVoiceOutputMode(mode)
	if !ok {
		return fmt.Errorf("invalid voice output mode: %q", mode)
	}
	if r.Store == nil {
		return errors.New("settings store is unavailable")
	}
	return r.Store.SetSetting(ctx, telegramVoiceOutputModeSettingKey(sessionKey), normalized)
}

func telegramVoiceOutputModeSettingKey(sessionKey string) string {
	return settingTelegramVoiceOutputModeKey + ":" + strings.TrimSpace(sessionKey)
}

func formatLiveToolEvent(ev agent.ToolEvent) string {
	label := liveToolLabel(ev)
	if ev.Phase == agent.ToolEventStart {
		if label != ev.Name {
			return fmt.Sprintf("tool start %d: %s", ev.Index, label)
		}
		return fmt.Sprintf("tool start %d: %s args=%s", ev.Index, label, truncateForLive(ev.ArgsJSON))
	}
	if strings.TrimSpace(ev.Error) != "" {
		return fmt.Sprintf("tool done %d: %s (%d ms) error=%s", ev.Index, label, ev.DurationMs, truncateForLive(ev.Error))
	}
	return fmt.Sprintf("tool done %d: %s (%d ms) result=%s", ev.Index, label, ev.DurationMs, truncateForLive(ev.ResultJSON))
}

func liveToolLabel(ev agent.ToolEvent) string {
	if !strings.EqualFold(strings.TrimSpace(ev.Name), "bash") {
		return ev.Name
	}
	cmd := bashCommandFromArgs(ev.ArgsJSON)
	if cmd == "" {
		return ev.Name
	}
	return fmt.Sprintf("bash [%s]", truncateForLive(cmd))
}

func bashCommandFromArgs(argsJSON string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	cmd, ok := args["command"].(string)
	if !ok {
		return ""
	}
	return strings.Join(strings.Fields(strings.TrimSpace(cmd)), " ")
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

func formatThinkingTrace(trace []agent.ThinkingTraceEntry) string {
	if len(trace) == 0 {
		return "thinking trace: (none)"
	}
	lines := []string{"thinking trace:"}
	for i, entry := range trace {
		mode := "final"
		if entry.ToolCallCount > 0 {
			mode = fmt.Sprintf("tool-step (%d tool calls)", entry.ToolCallCount)
		}
		thinking := strings.Join(strings.Fields(strings.TrimSpace(entry.Thinking)), " ")
		thinking = truncateForLive(thinking)
		lines = append(lines, fmt.Sprintf("%d. step=%d %s: %s", i+1, entry.Step, mode, thinking))
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
		if strings.Contains(cmdLower, "telegram") && (strings.Contains(cmdLower, " bot") || strings.Contains(cmdLower, " getupdates ")) {
			out = append(out, pollerCandidate{pid: pid, cmd: cmd})
		}
	}
	return out
}

func (r *Runner) requestBashApproval(ctx context.Context, b *bot.Bot, chatID, userID int64, sessionKey, command, normalized, reason string, allowAlways bool) error {
	normalized = strings.TrimSpace(normalized)
	if allowAlways && normalized != "" && r.isTelegramBashAlwaysAllowed(ctx, chatID, userID, normalized) {
		r.logf("bash approval bypassed (always allow match): chat=%d user=%d normalized=%q", chatID, userID, normalized)
		return nil
	}

	id := strconv.FormatUint(r.nextApproval.Add(1), 36)
	now := time.Now().UTC()
	entry := &pendingApproval{
		ID:          id,
		ChatID:      chatID,
		UserID:      userID,
		SessionKey:  sessionKey,
		Command:     strings.TrimSpace(command),
		Normalized:  normalized,
		Reason:      strings.TrimSpace(reason),
		AllowAlways: allowAlways,
		CreatedAt:   now,
		ExpiresAt:   now.Add(approvalTTL),
		DecisionCh:  make(chan approvalDecision, 1),
	}

	r.approvalMu.Lock()
	if r.approvals == nil {
		r.approvals = map[string]*pendingApproval{}
	}
	r.approvals[id] = entry
	r.approvalMu.Unlock()

	choiceHint := "Tap Allow, Always allow, or Deny."
	keyboardRows := [][]models.InlineKeyboardButton{
		{
			{Text: "Allow", CallbackData: formatApprovalCallback("allow", entry.ID)},
			{Text: "Always allow", CallbackData: formatApprovalCallback("always", entry.ID)},
		},
		{
			{Text: "Deny", CallbackData: formatApprovalCallback("deny", entry.ID)},
		},
	}
	if !entry.AllowAlways {
		choiceHint = "Tap Allow or Deny. Always allow is disabled for this command."
		keyboardRows = [][]models.InlineKeyboardButton{
			{
				{Text: "Allow", CallbackData: formatApprovalCallback("allow", entry.ID)},
				{Text: "Deny", CallbackData: formatApprovalCallback("deny", entry.ID)},
			},
		}
	}
	text := fmt.Sprintf("Command requires approval.\nReason: %s\nID: %s\n\nCommand:\n%s\n\n%s",
		entry.Reason,
		entry.ID,
		truncateApprovalCommand(entry.Command),
		choiceHint,
	)
	kb := &models.InlineKeyboardMarkup{
		InlineKeyboard: keyboardRows,
	}
	msg, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: kb,
	})
	if err != nil {
		r.deletePendingApproval(entry.ID)
		return fmt.Errorf("approval prompt send failed: %w", r.redactError(err))
	}
	if msg != nil {
		entry.MessageID = msg.ID
	}
	r.logf("bash approval requested: id=%s chat=%d user=%d reason=%q", entry.ID, chatID, userID, entry.Reason)

	timer := time.NewTimer(approvalTTL)
	defer timer.Stop()
	select {
	case decision := <-entry.DecisionCh:
		if decision == approvalDecisionAllow || decision == approvalDecisionAllowAlways {
			r.logf("bash approval granted: id=%s chat=%d decision=%s", entry.ID, chatID, approvalDecisionLabel(decision))
			return nil
		}
		return fmt.Errorf("command denied via Telegram approval")
	case <-ctx.Done():
		r.deletePendingApproval(entry.ID)
		r.markApprovalMessage(ctx, b, entry, "Approval canceled (request context ended).")
		return ctx.Err()
	case <-timer.C:
		r.deletePendingApproval(entry.ID)
		r.markApprovalMessage(ctx, b, entry, "Approval expired.")
		return fmt.Errorf("command approval timed out")
	}
}

func (r *Runner) handleCallbackQuery(ctx context.Context, b *bot.Bot, cq *models.CallbackQuery) {
	if cq == nil {
		return
	}
	action, approvalID, ok := parseApprovalCallback(cq.Data)
	if !ok {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cq.ID,
			Text:            "Unknown action.",
		})
		return
	}
	chatID, _, hasMessage := callbackQueryChatInfo(cq)
	userID := cq.From.ID
	if !r.isAllowlistedOwner(chatID, userID) {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cq.ID,
			Text:            "Unauthorized.",
			ShowAlert:       true,
		})
		r.logf("approval callback unauthorized: id=%s chat=%d user=%d", approvalID, chatID, userID)
		return
	}

	decision := approvalDecisionFromAction(action)
	entry, err := r.resolvePendingApproval(approvalID, decision, chatID, userID)
	if err != nil {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
			CallbackQueryID: cq.ID,
			Text:            err.Error(),
			ShowAlert:       true,
		})
		return
	}
	answerText := "Denied."
	statusText := "Denied."
	showAlert := false
	switch decision {
	case approvalDecisionAllow:
		answerText = "Allowed."
		statusText = "Allowed once. Executing command."
	case approvalDecisionAllowAlways:
		answerText = "Always-allow saved."
		statusText = "Always allow saved. Executing command."
		if !entry.AllowAlways {
			answerText = "Allowed once."
			statusText = "Allowed once. Always allow is disabled for this command."
		} else if err := r.persistTelegramBashAlwaysAllow(ctx, entry.ChatID, entry.UserID, entry.Normalized); err != nil {
			r.logf("failed to persist always-allow approval: id=%s chat=%d user=%d error=%v", entry.ID, entry.ChatID, entry.UserID, r.redactError(err))
			answerText = "Allowed once (failed to save always-allow)."
			statusText = "Allowed once. Failed to save always allow."
			showAlert = true
		}
	}
	if hasMessage {
		r.markApprovalMessage(ctx, b, entry, statusText)
	}
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID,
		Text:            answerText,
		ShowAlert:       showAlert,
	})
}

func (r *Runner) resolvePendingApproval(id string, decision approvalDecision, chatID, userID int64) (*pendingApproval, error) {
	r.approvalMu.Lock()
	entry, ok := r.approvals[id]
	if !ok {
		r.approvalMu.Unlock()
		return nil, fmt.Errorf("approval not found or already resolved")
	}
	if entry.ChatID != chatID || entry.UserID != userID {
		r.approvalMu.Unlock()
		return nil, fmt.Errorf("approval identity mismatch")
	}
	if time.Now().UTC().After(entry.ExpiresAt) {
		delete(r.approvals, id)
		r.approvalMu.Unlock()
		return nil, fmt.Errorf("approval expired")
	}
	delete(r.approvals, id)
	r.approvalMu.Unlock()
	select {
	case entry.DecisionCh <- decision:
	default:
	}
	return entry, nil
}

func (r *Runner) deletePendingApproval(id string) {
	r.approvalMu.Lock()
	delete(r.approvals, id)
	r.approvalMu.Unlock()
}

func (r *Runner) markApprovalMessage(ctx context.Context, b *bot.Bot, entry *pendingApproval, status string) {
	if entry == nil || entry.MessageID == 0 {
		return
	}
	text := fmt.Sprintf("Command approval (%s)\nID: %s\nReason: %s\n\nCommand:\n%s",
		status,
		entry.ID,
		entry.Reason,
		truncateApprovalCommand(entry.Command),
	)
	_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    entry.ChatID,
		MessageID: entry.MessageID,
		Text:      text,
	})
}

func (r *Runner) isAllowlistedOwner(chatID, userID int64) bool {
	return r.Cfg.Telegram.OwnerChatID == chatID && r.Cfg.Telegram.OwnerUserID == userID
}

func callbackQueryChatInfo(cq *models.CallbackQuery) (chatID int64, messageID int, ok bool) {
	if cq == nil {
		return 0, 0, false
	}
	if cq.Message.Message != nil {
		return cq.Message.Message.Chat.ID, cq.Message.Message.ID, true
	}
	if cq.Message.InaccessibleMessage != nil {
		return cq.Message.InaccessibleMessage.Chat.ID, cq.Message.InaccessibleMessage.MessageID, true
	}
	return 0, 0, false
}

func formatApprovalCallback(action, id string) string {
	return fmt.Sprintf("%s:%s:%s", approvalCallbackPrefix, action, id)
}

func parseApprovalCallback(data string) (action, id string, ok bool) {
	parts := strings.Split(strings.TrimSpace(data), ":")
	if len(parts) != 3 {
		return "", "", false
	}
	if parts[0] != approvalCallbackPrefix {
		return "", "", false
	}
	switch parts[1] {
	case "allow", "always", "deny":
	default:
		return "", "", false
	}
	if strings.TrimSpace(parts[2]) == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func approvalDecisionFromAction(action string) approvalDecision {
	switch strings.TrimSpace(strings.ToLower(action)) {
	case "allow":
		return approvalDecisionAllow
	case "always":
		return approvalDecisionAllowAlways
	default:
		return approvalDecisionDeny
	}
}

func approvalDecisionLabel(decision approvalDecision) string {
	switch decision {
	case approvalDecisionAllow:
		return "allow"
	case approvalDecisionAllowAlways:
		return "always"
	default:
		return "deny"
	}
}

func (r *Runner) isTelegramBashAlwaysAllowed(ctx context.Context, chatID, userID int64, normalized string) bool {
	if r.Store == nil {
		return false
	}
	key := telegramBashAlwaysAllowSettingKey(chatID, userID, normalized)
	_, ok, err := r.Store.GetSetting(ctx, key)
	if err != nil {
		r.logf("failed to read always-allow setting: key=%s error=%v", key, r.redactError(err))
		return false
	}
	return ok
}

func (r *Runner) persistTelegramBashAlwaysAllow(ctx context.Context, chatID, userID int64, normalized string) error {
	if r.Store == nil {
		return fmt.Errorf("settings store unavailable")
	}
	key := telegramBashAlwaysAllowSettingKey(chatID, userID, normalized)
	return r.Store.SetSetting(ctx, key, time.Now().UTC().Format(time.RFC3339Nano))
}

func telegramBashAlwaysAllowSettingKey(chatID, userID int64, normalized string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(normalized)))
	return fmt.Sprintf("%s:%d:%d:%x", settingTelegramBashAlwaysAllowKey, chatID, userID, hash[:])
}

func truncateApprovalCommand(cmd string) string {
	compact := strings.TrimSpace(cmd)
	if len(compact) <= maxApprovalCommandPreview {
		return compact
	}
	return compact[:maxApprovalCommandPreview-3] + "..."
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
