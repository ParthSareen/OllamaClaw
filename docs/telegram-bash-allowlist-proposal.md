# Telegram Bash Safety Proposal

## Goal
Reduce risk from prompt injection and accidental destructive actions when OllamaClaw is used via Telegram, while preserving fast execution for safe/read-only commands.

## Current Risk
Telegram messages can lead to model-issued `bash` tool calls. Existing guards only block a small set of lifecycle commands. This leaves broad command execution available from a remote chat channel.

## Proposed Model
Introduce a three-tier policy for `bash` commands in Telegram sessions:

1. `ALLOW` (auto-run)
- Read-only and low-risk commands.
- Examples: `ls`, `pwd`, `cat`, `head`, `tail`, `grep`, `find`, `stat`, `wc`, `git status`, `git diff`.

2. `REQUIRE_APPROVAL` (human gate)
- Mutating, networked, privileged, or process-control commands.
- Examples: `rm`, `mv`, `cp` (outside temp dirs), `chmod`, `chown`, `git commit`, `git push`, package installs, `curl|bash`, background jobs, process kill/start, docker/k8s actions.

3. `DENY` (never run from Telegram)
- High-risk classes that should be permanently blocked in chat.
- Examples: credential exfiltration patterns, lock-file tampering, nested launch/poller management, broad destructive patterns (`rm -rf /`, wildcard deletes in home/system paths), privilege escalation (`sudo`).

## Command Classifier
Add a deterministic policy engine before execution:
- Normalize command text (trim, collapse whitespace, lowercase for matching while preserving original).
- Tokenize shell operators (`&&`, `||`, `;`, pipes, redirects).
- Evaluate against ordered rules:
  1. Deny-list regex/pattern rules.
  2. Exact/anchored allowlist rules.
  3. Fallback to `REQUIRE_APPROVAL`.

Important: default should be `REQUIRE_APPROVAL`, not `ALLOW`.

## Telegram Approval Flow
When a command is classified as `REQUIRE_APPROVAL`:
- Do not execute immediately.
- Persist a pending approval record with:
  - `approval_id` (short id)
  - `chat_id`, `user_id`
  - exact command
  - normalized command hash
  - reason/category
  - created_at, expires_at (e.g., 10 minutes)
  - status (`pending`, `approved`, `rejected`, `expired`)
- Respond with concise instructions:
  - `Command requires approval: <reason>`
  - `Approve with /approve <approval_id>`
  - `Reject with /reject <approval_id>`

Approval commands:
- `/approve <id>` executes only the exact stored command (hash must match; no edits).
- `/reject <id>` marks rejected and never executes.
- `/pending` lists pending requests (id, age, short preview, reason).

Safety properties:
- Approval must come from the configured allowlisted owner identity.
- One-time approval token (single use).
- Expired approvals cannot be used.
- Audited execution path marks "approved_by" and timestamp.

## Data Model Additions
Add `command_approvals` table:
- `id TEXT PRIMARY KEY`
- `transport TEXT NOT NULL`
- `session_key TEXT NOT NULL`
- `chat_id INTEGER NOT NULL`
- `user_id INTEGER NOT NULL`
- `command TEXT NOT NULL`
- `command_hash TEXT NOT NULL`
- `reason TEXT NOT NULL`
- `status TEXT NOT NULL`
- `created_at TEXT NOT NULL`
- `expires_at TEXT NOT NULL`
- `resolved_at TEXT NULL`
- `resolved_by_user_id INTEGER NULL`

Optional index:
- `(status, expires_at)` for cleanup/listing.

## Tooling/Agent Integration
For `bash` in Telegram context:
- `ALLOW` => execute now.
- `REQUIRE_APPROVAL` => return structured tool error payload:
  - `{ "requires_approval": true, "approval_id": "...", "reason": "..." }`
- `DENY` => return structured deny payload:
  - `{ "denied": true, "reason": "..." }`

The assistant message should explain next step when approval is required.

## Logging and Audit
- Log policy decisions with command previews, never full sensitive payloads.
- Log approval lifecycle events (`created`, `approved`, `rejected`, `expired`, `executed`).
- Keep logs token-redacted and privacy-safe.

## Rollout Plan
1. Add classifier + `DENY` + `ALLOW` + fallback `REQUIRE_APPROVAL` (feature flag off by default).
2. Add approval storage and Telegram commands (`/approve`, `/reject`, `/pending`).
3. Enable feature flag for Telegram sessions.
4. Tune rules from observed false positives/negatives.

## Open Decisions
- Exact allowlist breadth for git/file ops.
- Approval TTL default (recommended: 10 min).
- Whether approvals are per-command only or can support narrow scopes (recommended: per-command only for v1).

## Recommended Defaults (v1)
- Default classification: `REQUIRE_APPROVAL`.
- `DENY` includes privilege escalation, lifecycle control, and destructive filesystem wildcards.
- Approval TTL: 10 minutes.
- One-time approvals only.
- Telegram only; REPL behavior unchanged.
