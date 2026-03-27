# OllamaClaw

OllamaClaw is a Telegram-first Go coding agent which uses Ollama. Hacking on this to use as a playground for different ideas and experiements. Currently using it to run crons, reminders, and small tasks.

It supports:
- Shared agent core for `repl` and `telegram` modes
- Built-in tools: `bash`, `read_file`, `write_file`, `web_search`, `web_fetch`
- Local SQLite persistence with per-chat sessions
- Context compaction (summary + recent turns)
- Shareable subprocess plugins (JSON-RPC over stdio)

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
`launch` prints live runtime logs (updates, commands, tool calls, cron output, and errors) to stdout.

Optional:

```bash
./ollamaclaw repl --model kimi-k2.5:cloud
```

## CLI

```bash
ollamaclaw repl [--model <name>]
ollamaclaw launch
ollamaclaw configure
ollamaclaw telegram init [--token <telegram-bot-token>] [--owner-id <id>] [--owner-chat-id <id>] [--owner-user-id <id>]
ollamaclaw telegram run   # legacy alias for launch

ollamaclaw plugin new <name>
ollamaclaw plugin test [--path <dir>]
ollamaclaw plugin pack [--path <dir>]
ollamaclaw plugin install <git|url|path>
ollamaclaw plugin list
ollamaclaw plugin enable <plugin-id>
ollamaclaw plugin disable <plugin-id>
ollamaclaw plugin remove <plugin-id>
ollamaclaw plugin update [plugin-id]
```

## Telegram commands

- `/start` shows onboarding/help text
- `/help` shows usage
- `/model [name]` shows/sets per-chat model
- `/tools` lists built-in + enabled plugin tools
- `/verbose [on|off]` enables/disables tool-call tracing for this chat session
- `/status` shows model, token counters, compactions, enabled plugin count, DB path
- `/reset` archives current session and starts a fresh one

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

`web_search` and `web_fetch` use Ollama hosted APIs and require:

```bash
export OLLAMA_API_KEY=...
```

## Config

File: `~/.ollamaclaw/config.json`

Defaults:

```json
{
  "ollama_host": "http://localhost:11434",
  "default_model": "kimi-k2.5:cloud",
  "db_path": "~/.ollamaclaw/state.db",
  "compaction_threshold": 0.8,
  "keep_recent_turns": 8,
  "context_window_tokens": 128000,
  "tool_output_max_bytes": 16384,
  "bash_timeout_seconds": 120,
  "plugin_call_timeout_sec": 60,
  "telegram": {
    "bot_token": "",
    "owner_chat_id": 0,
    "owner_user_id": 0
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
- `plugins`
- `plugin_tools`

Compaction archives old rows (`archived=1`) and keeps raw history in SQLite.

## Compaction behavior

- Trigger: prompt token count from Ollama exceeds configured threshold (`context_window_tokens * compaction_threshold`)
- Action: summarize older unarchived history using Ollama
- Result: save summary in `compactions`, archive old messages, keep recent turns active
- Active prompt: `system + latest summary + unarchived recent messages`

## Plugin system (v1)

Runtime:
- subprocess per call
- JSON-RPC 2.0 over stdio
- NDJSON framing (one JSON object per line)

Required RPC methods:
- `initialize`
- `tools/list`
- `tools/call`
- `shutdown`

Manifest file: `claw.plugin.json`

Example:
```json
{
  "id": "acme.echo",
  "name": "Echo",
  "version": "0.1.0",
  "apiVersion": "1.0",
  "entrypoint": {"command": "python3", "args": ["plugin.py"]},
  "protocol": {"jsonrpc": "2.0", "transport": "stdio", "framing": "ndjson"},
  "permissions": {"filesystem": ["read:*", "write:*"]}
}
```

Install sources:
- local path
- git source (`git:` prefix optional)
- archive URL (`.zip`, `.tar.gz`, `.tgz`)

Lock file: `~/.ollamaclaw/plugins.lock.json`
- Stores `id`, `version`, `source`, resolved ref, checksum, install path
- Checksum verified when loading enabled plugin tools

## Plugin onboarding

Create starter plugin:

```bash
./ollamaclaw plugin new my-plugin
```

Validate handshake + tool listing:

```bash
./ollamaclaw plugin test --path my-plugin
```

Pack for sharing:

```bash
./ollamaclaw plugin pack --path my-plugin
```

Install shared plugin:

```bash
./ollamaclaw plugin install <git|url|path>
```

## Development

```bash
go test ./...
go build ./...
```
