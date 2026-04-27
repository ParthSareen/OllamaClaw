# OllamaClaw

OllamaClaw is a private Telegram-first local agent for your laptop, powered by Ollama. It is built for remote coding/ops work from a trusted DM: ask it questions, let it inspect files/logs, run safe shell commands, follow up on GitHub/CI, and run reminder-triggered tasks while keeping state locally.
Current app version: `0.3.0`.

It supports:
- Private Telegram DM gateway with owner allowlist, image input, `/stop`, `/restart`, `/status`, `/fullsystem`, live tool visibility, and thinking controls
- Shared local agent core for `repl`, Telegram, reminders, and GitHub webhook-triggered turns
- Built-in tools only: `bash`, `read_file`, `write_file`, `web_search`, `web_fetch`, `read_logs`, reminder tools, subagent tools, and managed system prompt tools
- Background Codex subagents for durable, parallel tasks, including report-only GitHub PR review jobs via `gh`
- Reminder-first scheduling in `America/Los_Angeles`, backed by cron internally, with safe-mode and fresh prefetch context
- Local SQLite persistence with per-chat sessions, message history, compaction summaries, reminders, and learned prefetch commands
- Dynamic system prompt files plus background core memories (“dreaming”) injected into prompt context
- Context compaction plus a prompt-size safety guard before Ollama calls
- Optional GitHub webhook trigger path for proactive Telegram updates
- Optional localhost-only Hammerspoon hotkeys for text prompts and hold-to-talk voice input

## Install

```bash
# install from source repo
go install github.com/ParthSareen/OllamaClaw@latest

# or build locally in the repo
go build -o ollamaclaw .
```

## Quickstart

### 1) Launch (auto-onboarding)

```bash
./ollamaclaw launch
```

If config is missing, OllamaClaw opens an interactive setup UI.
It will ask for:
- Ollama host
- Default model
- Telegram bot token
- Telegram owner ID
- GitHub webhook owner login (optional)
- GitHub webhook secret (optional)
- GitHub webhook listen addr (optional)
- GitHub repo allowlist (optional, comma-separated)

The owner ID is used for both `owner_chat_id` and `owner_user_id`.

### 2) Update config later

```bash
./ollamaclaw configure
```

### 3) Run REPL mode

```bash
./ollamaclaw repl
```

The bot only handles **private chats** and only responds to the configured owner allowlist.
`launch` prints live runtime logs (updates, commands, tool calls, reminder output, and errors) to stdout.

Optional:

```bash
./ollamaclaw repl --model kimi-k2.5:cloud
```

## Voice prerequisites

Telegram voice notes and local hold-to-talk use local Gemma 4 audio transcription plus Kokoro speech synthesis. The default config expects:

- Ollama running at `http://localhost:11434`
- `gemma4:e2b` available locally for speech-to-text
- `ffmpeg` available on `PATH`
- Kokoro installed at `~/.ollamaclaw/kokoro-test/venv/bin/python`

OllamaClaw prompts Gemma for a clean, punctuated transcript and gives it Edith-specific technical context so ambiguous speech is biased toward coding, logs, models, repos, terminals, and local automation.

### 1) Install ffmpeg

```bash
brew install ffmpeg
```

### 2) Pull the local audio transcription model

```bash
ollama pull gemma4:e2b
ollama show gemma4:e2b
```

Confirm the `Capabilities` output includes `audio`.

Optional smoke test:

```bash
say -o /tmp/ollamaclaw_voice_test.aiff "OllamaClaw voice test."
afconvert -f WAVE -d LEI16@16000 /tmp/ollamaclaw_voice_test.aiff /tmp/ollamaclaw_voice_test.wav
ollama run gemma4:e2b --think false --hidethinking /tmp/ollamaclaw_voice_test.wav "Transcribe the speech in this audio. Output only a clean, punctuated transcript."
```

### 3) Set up Kokoro

```bash
mkdir -p ~/.ollamaclaw/kokoro-test
python3 -m venv ~/.ollamaclaw/kokoro-test/venv
~/.ollamaclaw/kokoro-test/venv/bin/python -m pip install --upgrade pip setuptools wheel
~/.ollamaclaw/kokoro-test/venv/bin/python -m pip install 'kokoro>=0.9.4' soundfile
```

Optional smoke test:

```bash
~/.ollamaclaw/kokoro-test/venv/bin/python - <<'PY'
from kokoro import KPipeline
import numpy as np
import soundfile as sf

pipeline = KPipeline(lang_code="a", repo_id="hexgrad/Kokoro-82M")
chunks = []
for _, _, audio in pipeline("Kokoro is ready for OllamaClaw voice replies.", voice="af_heart", speed=1.0):
    chunks.append(np.asarray(audio, dtype=np.float32))
sf.write("/tmp/ollamaclaw_kokoro_test.wav", np.concatenate(chunks), 24000)
PY
afplay /tmp/ollamaclaw_kokoro_test.wav
```

If you install Kokoro somewhere else, update `voice.kokoro_python` in `~/.ollamaclaw/config.json`. If Python package imports fail on Apple Silicon with an architecture mismatch, recreate the venv with an arm64 Python, or run the setup commands through `arch -arm64`.

### 4) Verify OllamaClaw's live audio path
```bash
OLLAMACLAW_LIVE_KOKORO_TEST=1 go test ./internal/audio -run TestSynthesizeLiveWhenEnabled -count=1
OLLAMACLAW_LIVE_OLLAMA_AUDIO_TEST=1 go test ./internal/audio -run TestTranscribeLiveWhenEnabled -count=1
```

## Local hotkeys

`ollamaclaw launch` starts a localhost-only control server by default at `127.0.0.1:8790`. It creates a local auth token at `~/.ollamaclaw/local_control.token`.

The MVP Hammerspoon script lives at `contrib/hammerspoon/ollamaclaw.lua` and defaults to:

- `Ctrl+Space`: local text prompt; echoes `local input:` to Telegram and replies in Telegram text.
- `Ctrl+Shift+Space`: hold-to-talk; records while held, sends the transcript to Telegram, replies in Telegram text, and speaks the final answer on the Mac.

Mac playback is only used for local hold-to-talk final assistant answers. Telegram text messages do not trigger voice synthesis; `/voice` controls replies to Telegram voice-note turns separately.

### Hammerspoon setup

Install Hammerspoon, then add this to `~/.hammerspoon/init.lua`:

```lua
dofile("/path/to/OllamaClaw/contrib/hammerspoon/ollamaclaw.lua")
```

Reload Hammerspoon after `ollamaclaw launch` has generated the token.

macOS permissions:

- Allow Hammerspoon in System Settings -> Privacy & Security -> Accessibility.
- Allow Hammerspoon microphone access when prompted.
- If `Ctrl+Space` is already used for input source switching, disable that shortcut in System Settings or edit the script bindings.

List ffmpeg microphone devices:

```bash
ffmpeg -f avfoundation -list_devices true -i ""
```

Then edit `audioInput` in the Hammerspoon script if needed. The bundled script defaults to `":1"` for audio device 1 and no video device; on this Mac, that is the built-in MacBook Pro microphone.

Hotkey voice output:

```text
/voice output mac
/voice output telegram
/voice output both
```

## CLI

```bash
ollamaclaw repl [--model <name>]
ollamaclaw launch
ollamaclaw configure
ollamaclaw telegram init [--token <telegram-bot-token>] [--owner-id <id>] [--owner-chat-id <id>] [--owner-user-id <id>]
ollamaclaw telegram run   # legacy alias for launch
```

## Telegram commands

- `/start` shows onboarding/help text
- `/help` shows usage
- `/model [name]` shows/sets per-chat model
- `/tools` lists built-in tools
- `/reminder list [active|all]` lists reminders
- All reminder schedules and timestamps are interpreted in `America/Los_Angeles` (PST/PDT)
- `/reminder safe <id>` marks a reminder as safe (Telegram bash approvals auto-approve for that reminder)
- `/reminder unsafe <id>` removes safe mode from a reminder
- `/reminder prefetch list <id>` shows learned prefetched commands for a reminder
- Reminders auto-learn stable bash commands from prior runs and prefetch them on future runs (`auto_prefetch` on by default)
- Prefetched commands are executed immediately before each reminder agent turn and injected as synthetic `bash` tool-call context with `run_id`, `run_started_at`, and per-command `fetched_at` timestamps; only the current run's `run_id` context is visible to the model
- Telegram bash policy defaults to allow for non-destructive commands; potentially destructive commands require approval; critical lifecycle commands remain blocked
- `/show tools [on|off]` toggles live tool event messages
- `/show thinking [on|off]` toggles thinking visibility mode
- `/show dreaming [on|off]` toggles background long-term-memory (“dreaming”) event notifications for this chat (default: on)
- `/verbose [on|off]` enables/disables tool + thinking traces for this chat session
- `/think [on|off|low|medium|high|xhigh|default]` shows/sets think value
- `/voice [off|text|audio|both]` shows/sets replies for Telegram voice-note turns (default: `both`)
- `/voice output [mac|telegram|both]` shows/sets local hotkey voice output (default: `mac`)
- `/dream` manually triggers a core-memory refresh for the current chat session
- `/status` shows model, estimated next prompt size (`len(request_json)/4`), dreaming notification state, lifetime token counters, compaction thresholds, last compaction snapshot, DB path
- `/fullsystem` shows the exact system context currently injected (system prompt + core memories + latest conversation summary)
- `/reset` archives current session and starts a fresh one
- `/stop` interrupts the active turn
- `/restart` restarts the launch loop from Telegram
- Send photos (or image documents) with an optional caption; image bytes are fetched from Telegram and forwarded to Ollama chat `images`
- Send voice notes to transcribe locally with Gemma 4; `/voice [off|text|audio|both]` controls whether replies include Telegram voice notes
- If messages arrive in quick succession, OllamaClaw waits for a 1.5s quiet window, coalesces them with newlines, then runs one turn

## GitHub webhook triggers

If `github_webhook.owner_login` and `github_webhook.secret` are configured, `ollamaclaw launch` starts a local webhook endpoint:

- `POST /webhooks/github` on `github_webhook.listen_addr` (default `127.0.0.1:8787`)
- Signature verified via `X-Hub-Signature-256` (HMAC-SHA256)
- Deduplicated by `X-GitHub-Delivery`
- Filtered to your configured GitHub login and optional repo allowlist
- Accepted events:
  - `pull_request`: `opened`, `synchronize`, `reopened`, `ready_for_review`
  - `pull_request_review`: `submitted`, `edited`, `dismissed`
  - `pull_request_review_comment`: `created`, `edited`
  - `check_run`: `completed`
  - `check_suite`: `completed`

Accepted webhook events are queued into your owner Telegram session as proactive turns.

## Built-in tools

### `bash`
Input:
```json
{"command":"ls -la","timeout_seconds":30}
```
Output:
```json
{"exit_code":0,"stdout":"...","stderr":""}
```

### `read_file`
Input:
```json
{"path":"/absolute/or/relative/path.txt"}
```
Output:
```json
{"path":"...","content":"..."}
```

### `write_file`
Input:
```json
{"path":"./notes.txt","content":"hello","create_dirs":true}
```
Output:
```json
{"path":"./notes.txt","bytes_written":5}
```

### `web_search`
Input:
```json
{"query":"latest ollama release","max_results":5}
```
Output:
```json
{"results":[{"title":"...","url":"...","content":"..."}]}
```

### `web_fetch`
Input:
```json
{"url":"https://ollama.com"}
```
Output:
```json
{"title":"...","content":"...","links":["..."]}
```

### `reminder_add`
Structured reminder creation in PST/PDT. Modes: `once`, `daily`, `interval`, `weekdays`, `monthly`.

### `reminder_list`
Lists reminders with normalized spec and compiled cron schedule.

### `reminder_remove`
Removes a reminder by `id`.

### `subagent_start`
Starts a durable headless Codex task in the background and returns a task id. Supports optional model, profile, sandbox, and `reasoning_effort`.

### `subagent_pr_review`
Queues one or more report-only Codex PR review tasks using isolated worktrees. Results are stored locally and never posted to GitHub automatically. Supports optional model, profile, sandbox, and `reasoning_effort`.

### `subagent_list`
Lists background Codex tasks.

### `subagent_status`
Shows lifecycle state, timing, repo/PR metadata, and artifact paths for a task.

### `subagent_result`
Reads the final stored report for a task.

### `subagent_cancel`
Cancels a queued or running background task.

### `read_logs`
Reads recent OllamaClaw runtime logs for self-debugging.

### `system_prompt_get`
Reads managed system prompt details (base/overlay paths, overlay content, optional revision history).

### `system_prompt_update`
Safely updates only the managed overlay (`set`, `append`, `clear`) with revision history logging.

### `system_prompt_history`
Lists recent managed overlay revisions.

### `system_prompt_rollback`
Rolls managed overlay back to a prior revision from `system_prompt_history`.

`web_search` and `web_fetch` use Ollama hosted APIs and require:

```bash
export OLLAMA_API_KEY=...
```

## Config

File: `~/.ollamaclaw/config.json`

Runtime system prompt file: `~/.ollamaclaw/system_prompt.txt` (read dynamically each turn; falls back to built-in prompt if missing/empty)
Managed system prompt overlay file: `~/.ollamaclaw/system_prompt.overlay.md` (agent-updatable layer)
Managed overlay history file: `~/.ollamaclaw/system_prompt.overlay.history.jsonl` (append-only revision log)
Core memories file: `~/.ollamaclaw/core_memories.md` (updated in background every 10 user turns and injected as a system context block)

Defaults:

```json
{
  "ollama_host": "http://localhost:11434",
  "default_model": "kimi-k2.5:cloud",
  "db_path": "~/.ollamaclaw/state.db",
  "log_path": "~/.ollamaclaw/ollamaclaw.log",
  "compaction_threshold": 0.8,
  "keep_recent_turns": 8,
  "context_window_tokens": 252000,
  "tool_output_max_bytes": 16384,
  "bash_timeout_seconds": 120,
  "telegram": {
    "bot_token": "",
    "owner_chat_id": 0,
    "owner_user_id": 0
  },
  "voice": {
    "transcription_model": "gemma4:e2b",
    "ollama_binary": "ollama",
    "ffmpeg_binary": "ffmpeg",
    "kokoro_python": "~/.ollamaclaw/kokoro-test/venv/bin/python",
    "kokoro_voice": "af_heart",
    "kokoro_lang_code": "a",
    "kokoro_speed": 1,
    "max_speech_chars": 1200
  },
  "github_webhook": {
    "enabled": false,
    "listen_addr": "127.0.0.1:8787",
    "secret": "",
    "owner_login": "",
    "repo_allowlist": []
  },
  "local_control": {
    "enabled": true,
    "listen_addr": "127.0.0.1:8790",
    "token_path": "~/.ollamaclaw/local_control.token"
  },
  "subagents": {
    "enabled": true,
    "codex_binary": "codex",
    "root_dir": "~/.ollamaclaw/subagents",
    "max_concurrent": 3,
    "default_timeout_minutes": 45,
    "default_model": "",
    "default_profile": "",
    "default_reasoning_effort": "xhigh",
    "sandbox": "workspace-write"
  }
}
```

## Persistence

SQLite database: `~/.ollamaclaw/state.db`

Tables:
- `settings`
- `sessions`
- `messages`
- `compactions`
- `cron_jobs`
- `cron_prefetch_commands`
- `subagent_tasks`

Compaction archives old rows (`archived=1`) and keeps raw history in SQLite.

## Compaction behavior

- Trigger: prompt token count from Ollama exceeds configured threshold (`context_window_tokens * compaction_threshold`)
- Action: summarize older unarchived history using Ollama
- Result: save summary in `compactions`, archive old messages, keep recent turns active
- Active prompt: `system + latest summary + unarchived recent messages`
- Telegram sends a compaction notice message when compaction happens during a turn (including background reminder-triggered turns sent to Telegram sessions)
- Safety guard: before every Ollama chat request, OllamaClaw estimates prompt size with `len(request_json)/4` and refuses requests over `context_window_tokens`

## Core memories behavior

- Trigger: every `10` user turns per session (`role=user` messages only)
- Telegram notifies the session when a background core-memory refresh starts/completes/fails (toggle with `/show dreaming on|off`)
- Dreaming completion notifications include a programmatic change summary (added/removed/kept items, char count delta, and short added/removed previews) with no extra LLM call
- Runs in background (non-blocking to active chat/reminder turn)
- Summarizes stable preferences/workflows/constraints from recent dialogue
- Writes to `~/.ollamaclaw/core_memories.md` using managed markers
- Enforces a hard cap of `4000` characters for stored/injected core memory content
- Injects managed core memories into prompt context as a dedicated system message

## Subagent behavior

- Subagents run headless Codex CLI jobs in the background and store artifacts under `~/.ollamaclaw/subagents/tasks/<id>/`
- Subagents default to Codex `model_reasoning_effort="xhigh"` unless the task supplies `reasoning_effort`
- PR review jobs use `gh` for read-only PR metadata, create isolated git worktrees under `~/.ollamaclaw/subagents/worktrees/`, and run `codex exec review`
- Telegram sends completion reports back to the originating chat; `/agents list`, `/agents show <id>`, and `/agents cancel <id>` provide manual control
- V1 is report-only for GitHub: agents must not post comments, submit reviews, push branches, or mutate GitHub state

## Development

```bash
go test ./...
go build ./...
```
