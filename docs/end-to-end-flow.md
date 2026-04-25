# OllamaClaw End-to-End Flow

This diagram reflects the current repo architecture across `launch`, Telegram turns, REPL turns, and scheduled cron runs.

```mermaid
flowchart TD
    A["`main.go`"] --> B["`cli.New().Run(args)`"]
    B --> C{"Mode / command"}
    C -->|`launch`| D["Bootstrap runtime"]
    C -->|`repl`| D
    C -->|`configure`| CFG["Interactive config flow"]

    D --> D1["Load config"]
    D1 --> D2["Open SQLite store"]
    D2 --> D3["Create Ollama client"]
    D3 --> D4["Create cron manager"]
    D4 --> D5["Create shared agent engine"]
    D5 --> D6["Wire cron runner -> engine"]
    D6 --> E{"Ingress"}

    E -->|Telegram| T0["Telegram runner"]
    E -->|REPL| R0["REPL loop"]
    E -->|Cron tick| C0["Cron manager"]

    T0 --> T1["Long-poll Telegram Bot API"]
    T1 --> T2["Private-chat + owner allowlist check"]
    T2 --> T3{"Slash command?"}
    T3 -->|Yes| T4["Handle `/model`, `/cron`, `/tools`, `/stop`, `/restart`, ..."]
    T3 -->|No| T5["Debounce/coalesce pending messages (1.5s)"]
    T5 --> T6["Fetch + base64-encode image attachments if present"]
    T6 --> H["`Engine.HandleTextWithOptions(...)`"]

    R0 --> H

    C0 --> C1["Load active jobs from SQLite"]
    C1 --> C2["On schedule: `runJob(jobID)`"]
    C2 --> C3["Optionally enable safe bash auto-approval"]
    C3 --> C4["Optionally prefetch learned bash commands"]
    C4 --> H

    subgraph Engine["Shared agent engine"]
        H --> H1["Get or create active session"]
        H1 --> H2["Store user message"]
        H2 --> H3["Build active prompt"]
        H3 --> H3A["System prompt + managed overlay + current time"]
        H3A --> H3B["Core memories + latest compaction summary"]
        H3B --> H3C["Unarchived session messages + current prefetch context"]
        H3C --> H4["Call Ollama chat with builtin tool schemas"]
        H4 --> H5{"Tool calls returned?"}
        H5 -->|Yes| H6["Execute built-in tools"]
        H6 --> H6A["`bash`, `read_file`, `write_file`, `read_logs`"]
        H6A --> H6B["`web_search`, `web_fetch`"]
        H6B --> H6C["`system_prompt_*`, `cron_*`"]
        H6C --> H7["Persist tool outputs"]
        H7 --> H3
        H5 -->|No| H8["Final assistant reply"]
        H8 --> H9["Maybe compact old context via Ollama summary"]
        H9 --> H10["Archive old messages + save compaction snapshot"]
        H10 --> H11["Maybe refresh core memories in background"]
    end

    H4 --> OLLAMA["Ollama chat model"]
    H9 --> OLLAMA
    H11 --> OLLAMA

    H6 --> HOST["Local host resources"]
    HOST --> HOST1["Shell commands / filesystem / logs"]
    H6B --> WEB["Web fetch/search APIs"]

    D1 --> FILES["`~/.ollamaclaw/*` files"]
    FILES --> FILES1["`config.json`"]
    FILES --> FILES2["`system_prompt.txt` + overlay history"]
    FILES --> FILES3["`core_memories.md`"]

    D2 --> DB[("`state.db`")]
    H1 --> DB
    H2 --> DB
    H7 --> DB
    H10 --> DB
    C1 --> DB
    C4 --> DB

    H11 --> OUT{"Output path"}
    H8 --> OUT
    OUT -->|Telegram turn| OT["Send chunked Telegram reply\nOptional Kokoro voice note\nOptional live tool/thinking trace"]
    OUT -->|REPL turn| OR["Print response in terminal"]
    OUT -->|Cron run| OC["Scheduler sink pushes output to Telegram session"]
```

## Notes

- Telegram, REPL, and cron all converge on the same `agent.Engine`.
- SQLite stores sessions, messages, compactions, cron jobs, and learned prefetch commands.
- Cron runs can inject prefetched `bash` outputs into the next model turn as synthetic tool context.
- Context compaction and core-memory refresh are both model-driven background maintenance steps.
