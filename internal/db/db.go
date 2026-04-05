package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	raw.SetMaxOpenConns(1)
	store := &Store{db: raw}
	if err := store.migrate(context.Background()); err != nil {
		raw.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);
`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	migrations := []string{
		`
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  transport TEXT NOT NULL,
  session_key TEXT NOT NULL,
  model_override TEXT NOT NULL,
  is_active INTEGER NOT NULL DEFAULT 1,
  total_prompt_tokens INTEGER NOT NULL DEFAULT 0,
  total_eval_tokens INTEGER NOT NULL DEFAULT 0,
  compaction_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_lookup ON sessions(transport, session_key, is_active);

CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  thinking TEXT NOT NULL DEFAULT '',
  tool_name TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT NOT NULL DEFAULT '',
  tool_args_json TEXT NOT NULL DEFAULT '',
  tool_calls_json TEXT NOT NULL DEFAULT '',
  prompt_eval_count INTEGER NOT NULL DEFAULT 0,
  eval_count INTEGER NOT NULL DEFAULT 0,
  archived INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  FOREIGN KEY(session_id) REFERENCES sessions(id)
);

CREATE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_messages_unarchived ON messages(session_id, archived, seq);

CREATE TABLE IF NOT EXISTS compactions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL,
  summary TEXT NOT NULL,
  first_kept_message_id INTEGER NOT NULL DEFAULT 0,
  archived_before_seq INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  FOREIGN KEY(session_id) REFERENCES sessions(id)
);

CREATE INDEX IF NOT EXISTS idx_compactions_session ON compactions(session_id, id DESC);

CREATE TABLE IF NOT EXISTS plugins (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  version TEXT NOT NULL,
  source TEXT NOT NULL,
  resolved_ref TEXT NOT NULL,
  checksum TEXT NOT NULL,
  install_path TEXT NOT NULL,
  permissions_json TEXT NOT NULL DEFAULT '{}',
  enabled INTEGER NOT NULL DEFAULT 1,
  installed_at TEXT NOT NULL,
  last_updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS plugin_tools (
  plugin_id TEXT NOT NULL,
  tool_name TEXT NOT NULL,
  description TEXT NOT NULL,
  schema_json TEXT NOT NULL,
  timeout_sec INTEGER NOT NULL DEFAULT 60,
  enabled INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(plugin_id, tool_name),
  FOREIGN KEY(plugin_id) REFERENCES plugins(id)
);
`,
		`
CREATE TABLE IF NOT EXISTS cron_jobs (
  id TEXT PRIMARY KEY,
  schedule TEXT NOT NULL,
  prompt TEXT NOT NULL,
  transport TEXT NOT NULL,
  session_key TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  last_run_at TEXT NULL,
  next_run_at TEXT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cron_jobs_active ON cron_jobs(active, next_run_at);
`,
		`
ALTER TABLE cron_jobs ADD COLUMN safe INTEGER NOT NULL DEFAULT 0;
`,
		`
ALTER TABLE cron_jobs ADD COLUMN auto_prefetch INTEGER NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS cron_prefetch_commands (
  job_id TEXT NOT NULL,
  command TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(job_id, command),
  FOREIGN KEY(job_id) REFERENCES cron_jobs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_cron_prefetch_job ON cron_prefetch_commands(job_id);
`,
	}

	for i, sqlText := range migrations {
		version := i + 1
		applied, err := s.isMigrationApplied(ctx, version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, sqlText); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}

	maxVersion := len(migrations)
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`)
	var seen int
	if err := row.Scan(&seen); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if seen > maxVersion {
		return fmt.Errorf("database schema version %d is newer than binary (%d)", seen, maxVersion)
	}
	return nil
}

func (s *Store) isMigrationApplied(ctx context.Context, version int) (bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version)
	var one int
	err := row.Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check migration %d: %w", version, err)
	}
	return true, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at
`, key, value, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key)
	var val string
	err := row.Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get setting %s: %w", key, err)
	}
	return val, true, nil
}

func (s *Store) GetOrCreateActiveSession(ctx context.Context, transport, sessionKey, defaultModel string) (Session, error) {
	sess, ok, err := s.GetActiveSession(ctx, transport, sessionKey)
	if err != nil {
		return Session{}, err
	}
	if ok {
		if sess.ModelOverride == "" {
			sess.ModelOverride = defaultModel
		}
		return sess, nil
	}
	return s.CreateSession(ctx, transport, sessionKey, defaultModel)
}

func (s *Store) GetActiveSession(ctx context.Context, transport, sessionKey string) (Session, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, transport, session_key, model_override, is_active, total_prompt_tokens, total_eval_tokens, compaction_count, created_at, updated_at
FROM sessions
WHERE transport = ? AND session_key = ? AND is_active = 1
ORDER BY created_at DESC
LIMIT 1
`, transport, sessionKey)
	var sess Session
	var active int
	var createdAt, updatedAt string
	err := row.Scan(&sess.ID, &sess.Transport, &sess.SessionKey, &sess.ModelOverride, &active, &sess.TotalPromptToken, &sess.TotalEvalToken, &sess.CompactionCount, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("get active session: %w", err)
	}
	sess.IsActive = active == 1
	sess.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	sess.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return sess, true, nil
}

func (s *Store) CreateSession(ctx context.Context, transport, sessionKey, model string) (Session, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := fmt.Sprintf("%s:%s:%d", transport, sessionKey, time.Now().UTC().UnixNano())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions(id, transport, session_key, model_override, is_active, created_at, updated_at)
VALUES(?, ?, ?, ?, 1, ?, ?)
`, id, transport, sessionKey, model, now, now)
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return Session{
		ID:            id,
		Transport:     transport,
		SessionKey:    sessionKey,
		ModelOverride: model,
		IsActive:      true,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}, nil
}

func (s *Store) SetSessionModel(ctx context.Context, sessionID, model string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET model_override = ?, updated_at = ? WHERE id = ?`, model, time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return fmt.Errorf("set session model: %w", err)
	}
	return nil
}

func (s *Store) ResetSession(ctx context.Context, transport, sessionKey, defaultModel string) (Session, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET is_active = 0, updated_at = ? WHERE transport = ? AND session_key = ? AND is_active = 1`, time.Now().UTC().Format(time.RFC3339Nano), transport, sessionKey)
	if err != nil {
		tx.Rollback()
		return Session{}, fmt.Errorf("deactivate session: %w", err)
	}
	now := time.Now().UTC()
	id := fmt.Sprintf("%s:%s:%d", transport, sessionKey, now.UnixNano())
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(id, transport, session_key, model_override, is_active, created_at, updated_at) VALUES(?, ?, ?, ?, 1, ?, ?)`, id, transport, sessionKey, defaultModel, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		tx.Rollback()
		return Session{}, fmt.Errorf("create reset session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, err
	}
	return Session{ID: id, Transport: transport, SessionKey: sessionKey, ModelOverride: defaultModel, IsActive: true, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) IncrementSessionTokens(ctx context.Context, sessionID string, promptTokens, evalTokens int) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE sessions
SET total_prompt_tokens = total_prompt_tokens + ?,
    total_eval_tokens = total_eval_tokens + ?,
    updated_at = ?
WHERE id = ?
`, promptTokens, evalTokens, time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return fmt.Errorf("increment session tokens: %w", err)
	}
	return nil
}

func (s *Store) IncrementCompactions(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET compaction_count = compaction_count + 1, updated_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return fmt.Errorf("increment compactions: %w", err)
	}
	return nil
}

func (s *Store) NextMessageSeq(ctx context.Context, sessionID string) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ?`, sessionID)
	var seq int
	if err := row.Scan(&seq); err != nil {
		return 0, fmt.Errorf("next message seq: %w", err)
	}
	return seq, nil
}

func (s *Store) CountMessagesByRole(ctx context.Context, sessionID, role string) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM messages WHERE session_id = ? AND role = ?`, sessionID, role)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count messages by role: %w", err)
	}
	return count, nil
}

func (s *Store) InsertMessage(ctx context.Context, m *Message) error {
	if m.Seq == 0 {
		seq, err := s.NextMessageSeq(ctx, m.SessionID)
		if err != nil {
			return err
		}
		m.Seq = seq
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO messages(session_id, seq, role, content, thinking, tool_name, tool_call_id, tool_args_json, tool_calls_json, prompt_eval_count, eval_count, archived, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, m.SessionID, m.Seq, m.Role, m.Content, m.Thinking, m.ToolName, m.ToolCallID, m.ToolArgsJSON, m.ToolCallsJSON, m.PromptEvalCount, m.EvalCount, boolToInt(m.Archived), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	id, _ := res.LastInsertId()
	m.ID = id
	return nil
}

func (s *Store) ListMessages(ctx context.Context, sessionID string, includeArchived bool) ([]Message, error) {
	query := `SELECT id, session_id, seq, role, content, thinking, tool_name, tool_call_id, tool_args_json, tool_calls_json, prompt_eval_count, eval_count, archived, created_at FROM messages WHERE session_id = ?`
	if !includeArchived {
		query += ` AND archived = 0`
	}
	query += ` ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var archived int
		var created string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Role, &m.Content, &m.Thinking, &m.ToolName, &m.ToolCallID, &m.ToolArgsJSON, &m.ToolCallsJSON, &m.PromptEvalCount, &m.EvalCount, &archived, &created); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Archived = archived == 1
		m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}
	return out, nil
}

func (s *Store) ArchiveMessagesByIDs(ctx context.Context, sessionID string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, sessionID)
	for _, id := range ids {
		args = append(args, id)
	}
	query := fmt.Sprintf(`UPDATE messages SET archived = 1 WHERE session_id = ? AND id IN (%s)`, placeholders)
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("archive messages: %w", err)
	}
	return nil
}

func (s *Store) InsertCompaction(ctx context.Context, c Compaction) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO compactions(session_id, summary, first_kept_message_id, archived_before_seq, created_at)
VALUES(?, ?, ?, ?, ?)
`, c.SessionID, c.Summary, c.FirstKeptMessage, c.ArchivedBeforeSeq, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert compaction: %w", err)
	}
	return nil
}

func (s *Store) LatestCompactionSummary(ctx context.Context, sessionID string) (string, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT summary FROM compactions WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sessionID)
	var summary string
	err := row.Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("latest compaction: %w", err)
	}
	return summary, true, nil
}

func (s *Store) UpsertPlugin(ctx context.Context, p Plugin) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO plugins(id, name, version, source, resolved_ref, checksum, install_path, permissions_json, enabled, installed_at, last_updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name=excluded.name,
  version=excluded.version,
  source=excluded.source,
  resolved_ref=excluded.resolved_ref,
  checksum=excluded.checksum,
  install_path=excluded.install_path,
  permissions_json=excluded.permissions_json,
  enabled=excluded.enabled,
  last_updated_at=excluded.last_updated_at
`, p.ID, p.Name, p.Version, p.Source, p.ResolvedRef, p.Checksum, p.InstallPath, p.Permissions, boolToInt(p.Enabled), now, now)
	if err != nil {
		return fmt.Errorf("upsert plugin: %w", err)
	}
	return nil
}

func (s *Store) GetPlugin(ctx context.Context, id string) (Plugin, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, name, version, source, resolved_ref, checksum, install_path, permissions_json, enabled, installed_at, last_updated_at FROM plugins WHERE id = ?`, id)
	var p Plugin
	var enabled int
	var installedAt, updatedAt string
	err := row.Scan(&p.ID, &p.Name, &p.Version, &p.Source, &p.ResolvedRef, &p.Checksum, &p.InstallPath, &p.Permissions, &enabled, &installedAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Plugin{}, false, nil
	}
	if err != nil {
		return Plugin{}, false, fmt.Errorf("get plugin: %w", err)
	}
	p.Enabled = enabled == 1
	p.InstalledAt, _ = time.Parse(time.RFC3339Nano, installedAt)
	p.LastUpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return p, true, nil
}

func (s *Store) ListPlugins(ctx context.Context, enabledOnly bool) ([]Plugin, error) {
	query := `SELECT id, name, version, source, resolved_ref, checksum, install_path, permissions_json, enabled, installed_at, last_updated_at FROM plugins`
	if enabledOnly {
		query += ` WHERE enabled = 1`
	}
	query += ` ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list plugins: %w", err)
	}
	defer rows.Close()
	var out []Plugin
	for rows.Next() {
		var p Plugin
		var enabled int
		var installedAt, updatedAt string
		if err := rows.Scan(&p.ID, &p.Name, &p.Version, &p.Source, &p.ResolvedRef, &p.Checksum, &p.InstallPath, &p.Permissions, &enabled, &installedAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan plugin: %w", err)
		}
		p.Enabled = enabled == 1
		p.InstalledAt, _ = time.Parse(time.RFC3339Nano, installedAt)
		p.LastUpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate plugins: %w", err)
	}
	return out, nil
}

func (s *Store) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE plugins SET enabled = ?, last_updated_at = ? WHERE id = ?`, boolToInt(enabled), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("set plugin enabled: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("plugin %s not found", id)
	}
	return nil
}

func (s *Store) DeletePlugin(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_tools WHERE plugin_id = ?`, id); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete plugin tools: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugins WHERE id = ?`, id); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete plugin: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) ReplacePluginTools(ctx context.Context, pluginID string, tools []PluginTool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plugin_tools WHERE plugin_id = ?`, pluginID); err != nil {
		tx.Rollback()
		return fmt.Errorf("clear plugin tools: %w", err)
	}
	for _, t := range tools {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO plugin_tools(plugin_id, tool_name, description, schema_json, timeout_sec, enabled, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?)
`, pluginID, t.ToolName, t.Description, t.SchemaJSON, t.TimeoutSec, boolToInt(t.Enabled), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			return fmt.Errorf("insert plugin tool %s: %w", t.ToolName, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) ListEnabledPluginTools(ctx context.Context) ([]PluginTool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT pt.plugin_id, pt.tool_name, pt.description, pt.schema_json, pt.timeout_sec, pt.enabled, pt.updated_at
FROM plugin_tools pt
JOIN plugins p ON p.id = pt.plugin_id
WHERE p.enabled = 1 AND pt.enabled = 1
ORDER BY pt.tool_name ASC
`)
	if err != nil {
		return nil, fmt.Errorf("list enabled plugin tools: %w", err)
	}
	defer rows.Close()
	var out []PluginTool
	for rows.Next() {
		var t PluginTool
		var enabled int
		var updated string
		if err := rows.Scan(&t.PluginID, &t.ToolName, &t.Description, &t.SchemaJSON, &t.TimeoutSec, &enabled, &updated); err != nil {
			return nil, fmt.Errorf("scan plugin tool: %w", err)
		}
		t.Enabled = enabled == 1
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate plugin tools: %w", err)
	}
	return out, nil
}

func (s *Store) ListPluginTools(ctx context.Context, pluginID string) ([]PluginTool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT plugin_id, tool_name, description, schema_json, timeout_sec, enabled, updated_at FROM plugin_tools WHERE plugin_id = ? ORDER BY tool_name`, pluginID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PluginTool
	for rows.Next() {
		var t PluginTool
		var enabled int
		var updated string
		if err := rows.Scan(&t.PluginID, &t.ToolName, &t.Description, &t.SchemaJSON, &t.TimeoutSec, &enabled, &updated); err != nil {
			return nil, err
		}
		t.Enabled = enabled == 1
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) UpsertCronJob(ctx context.Context, job CronJob) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lastRun := nullableTimeString(job.LastRunAt)
	nextRun := nullableTimeString(job.NextRunAt)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO cron_jobs(id, schedule, prompt, transport, session_key, active, safe, auto_prefetch, last_run_at, next_run_at, last_error, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  schedule=excluded.schedule,
  prompt=excluded.prompt,
  transport=excluded.transport,
  session_key=excluded.session_key,
  active=excluded.active,
  safe=excluded.safe,
  auto_prefetch=excluded.auto_prefetch,
  last_run_at=excluded.last_run_at,
  next_run_at=excluded.next_run_at,
  last_error=excluded.last_error,
  updated_at=excluded.updated_at
`, job.ID, job.Schedule, job.Prompt, job.Transport, job.SessionKey, boolToInt(job.Active), boolToInt(job.Safe), boolToInt(job.AutoPrefetch), lastRun, nextRun, job.LastError, now, now)
	if err != nil {
		return fmt.Errorf("upsert cron job: %w", err)
	}
	return nil
}

func (s *Store) GetCronJob(ctx context.Context, id string) (CronJob, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, schedule, prompt, transport, session_key, active, safe, auto_prefetch, last_run_at, next_run_at, last_error, created_at, updated_at
FROM cron_jobs
WHERE id = ?
`, id)
	job, ok, err := scanCronJob(row)
	if err != nil {
		return CronJob{}, false, err
	}
	return job, ok, nil
}

func (s *Store) ListCronJobs(ctx context.Context, activeOnly bool) ([]CronJob, error) {
	query := `
SELECT id, schedule, prompt, transport, session_key, active, safe, auto_prefetch, last_run_at, next_run_at, last_error, created_at, updated_at
FROM cron_jobs
`
	if activeOnly {
		query += ` WHERE active = 1`
	}
	query += ` ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list cron jobs: %w", err)
	}
	defer rows.Close()
	out := []CronJob{}
	for rows.Next() {
		var (
			job                    CronJob
			active, safe, prefetch int
			lastRunRaw, nextRaw    sql.NullString
			createdRaw, updatedRaw string
		)
		if err := rows.Scan(&job.ID, &job.Schedule, &job.Prompt, &job.Transport, &job.SessionKey, &active, &safe, &prefetch, &lastRunRaw, &nextRaw, &job.LastError, &createdRaw, &updatedRaw); err != nil {
			return nil, fmt.Errorf("scan cron job: %w", err)
		}
		job.Active = active == 1
		job.Safe = safe == 1
		job.AutoPrefetch = prefetch == 1
		job.LastRunAt = parseNullTime(lastRunRaw)
		job.NextRunAt = parseNullTime(nextRaw)
		job.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdRaw)
		job.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedRaw)
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cron jobs: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteCronJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete cron job: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("cron job %s not found", id)
	}
	return nil
}

func (s *Store) UpdateCronRun(ctx context.Context, id string, lastRun, nextRun *time.Time, lastError string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE cron_jobs
SET last_run_at = ?, next_run_at = ?, last_error = ?, updated_at = ?
WHERE id = ?
`, nullableTimeString(lastRun), nullableTimeString(nextRun), lastError, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("update cron job run: %w", err)
	}
	return nil
}

func scanCronJob(row *sql.Row) (CronJob, bool, error) {
	var (
		job                    CronJob
		active, safe, prefetch int
		lastRunRaw, nextRaw    sql.NullString
		createdRaw, updatedRaw string
	)
	err := row.Scan(&job.ID, &job.Schedule, &job.Prompt, &job.Transport, &job.SessionKey, &active, &safe, &prefetch, &lastRunRaw, &nextRaw, &job.LastError, &createdRaw, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return CronJob{}, false, nil
	}
	if err != nil {
		return CronJob{}, false, fmt.Errorf("scan cron job: %w", err)
	}
	job.Active = active == 1
	job.Safe = safe == 1
	job.AutoPrefetch = prefetch == 1
	job.LastRunAt = parseNullTime(lastRunRaw)
	job.NextRunAt = parseNullTime(nextRaw)
	job.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdRaw)
	job.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedRaw)
	return job, true, nil
}

func (s *Store) ListCronPrefetchCommands(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT command FROM cron_prefetch_commands WHERE job_id = ? ORDER BY command ASC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("list cron prefetch commands: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var cmd string
		if err := rows.Scan(&cmd); err != nil {
			return nil, fmt.Errorf("scan cron prefetch command: %w", err)
		}
		out = append(out, cmd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cron prefetch commands: %w", err)
	}
	return out, nil
}

func (s *Store) UpsertCronPrefetchCommands(ctx context.Context, jobID string, commands []string) error {
	if strings.TrimSpace(jobID) == "" {
		return fmt.Errorf("job id is required")
	}
	if len(commands) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO cron_prefetch_commands(job_id, command, created_at, updated_at)
VALUES(?, ?, ?, ?)
ON CONFLICT(job_id, command) DO UPDATE SET
  updated_at=excluded.updated_at
`, jobID, command, now, now); err != nil {
			tx.Rollback()
			return fmt.Errorf("upsert cron prefetch command: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func parseNullTime(v sql.NullString) *time.Time {
	if !v.Valid || strings.TrimSpace(v.String) == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	return &t
}

func nullableTimeString(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
