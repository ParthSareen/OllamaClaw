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
		`
ALTER TABLE cron_jobs ADD COLUMN reminder_mode TEXT NOT NULL DEFAULT 'legacy_cron';
ALTER TABLE cron_jobs ADD COLUMN reminder_spec_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE cron_jobs ADD COLUMN once_fire_at TEXT NULL;
UPDATE cron_jobs SET reminder_mode = 'legacy_cron' WHERE TRIM(COALESCE(reminder_mode, '')) = '';
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin insert message: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if m.Seq == 0 {
		row := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ?`, m.SessionID)
		if err := row.Scan(&m.Seq); err != nil {
			return fmt.Errorf("next message seq: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO messages(session_id, seq, role, content, thinking, tool_name, tool_call_id, tool_args_json, tool_calls_json, prompt_eval_count, eval_count, archived, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, m.SessionID, m.Seq, m.Role, m.Content, m.Thinking, m.ToolName, m.ToolCallID, m.ToolArgsJSON, m.ToolCallsJSON, m.PromptEvalCount, m.EvalCount, boolToInt(m.Archived), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit insert message: %w", err)
	}
	committed = true
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

func (s *Store) ArchiveMessagesByToolCallID(ctx context.Context, sessionID, toolCallID string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(toolCallID) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET archived = 1 WHERE session_id = ? AND archived = 0 AND tool_call_id = ?`, sessionID, toolCallID)
	if err != nil {
		return fmt.Errorf("archive messages by tool call id: %w", err)
	}
	return nil
}

func (s *Store) ArchiveMessagesByToolCallIDPrefix(ctx context.Context, sessionID, prefix string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(prefix) == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET archived = 1 WHERE session_id = ? AND archived = 0 AND tool_call_id LIKE ?`, sessionID, prefix+"%")
	if err != nil {
		return fmt.Errorf("archive messages by tool call id prefix: %w", err)
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

func (s *Store) LatestCompaction(ctx context.Context, sessionID string) (Compaction, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, session_id, summary, first_kept_message_id, archived_before_seq, created_at
FROM compactions
WHERE session_id = ?
ORDER BY id DESC
LIMIT 1
`, sessionID)
	var (
		c          Compaction
		createdRaw string
	)
	err := row.Scan(&c.ID, &c.SessionID, &c.Summary, &c.FirstKeptMessage, &c.ArchivedBeforeSeq, &createdRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return Compaction{}, false, nil
	}
	if err != nil {
		return Compaction{}, false, fmt.Errorf("latest compaction row: %w", err)
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdRaw)
	return c, true, nil
}

func (s *Store) UpsertReminderJob(ctx context.Context, job ReminderJob) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	lastRun := nullableTimeString(job.LastRunAt)
	nextRun := nullableTimeString(job.NextRunAt)
	onceFireAt := nullableTimeString(job.OnceFireAt)
	mode := strings.TrimSpace(job.ReminderMode)
	if mode == "" {
		mode = "legacy_cron"
	}
	specJSON := strings.TrimSpace(job.ReminderSpecJSON)
	if specJSON == "" {
		specJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO cron_jobs(id, schedule, prompt, transport, session_key, active, safe, auto_prefetch, reminder_mode, reminder_spec_json, once_fire_at, last_run_at, next_run_at, last_error, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  schedule=excluded.schedule,
  prompt=excluded.prompt,
  transport=excluded.transport,
  session_key=excluded.session_key,
  active=excluded.active,
  safe=excluded.safe,
  auto_prefetch=excluded.auto_prefetch,
  reminder_mode=excluded.reminder_mode,
  reminder_spec_json=excluded.reminder_spec_json,
  once_fire_at=excluded.once_fire_at,
  last_run_at=excluded.last_run_at,
  next_run_at=excluded.next_run_at,
  last_error=excluded.last_error,
  updated_at=excluded.updated_at
`, job.ID, job.Schedule, job.Prompt, job.Transport, job.SessionKey, boolToInt(job.Active), boolToInt(job.Safe), boolToInt(job.AutoPrefetch), mode, specJSON, onceFireAt, lastRun, nextRun, job.LastError, now, now)
	if err != nil {
		return fmt.Errorf("upsert reminder job: %w", err)
	}
	return nil
}

func (s *Store) UpsertCronJob(ctx context.Context, job CronJob) error {
	return s.UpsertReminderJob(ctx, job)
}

func (s *Store) GetReminderJob(ctx context.Context, id string) (ReminderJob, bool, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, schedule, prompt, transport, session_key, active, safe, auto_prefetch, reminder_mode, reminder_spec_json, once_fire_at, last_run_at, next_run_at, last_error, created_at, updated_at
FROM cron_jobs
WHERE id = ?
`, id)
	job, ok, err := scanReminderJob(row)
	if err != nil {
		return ReminderJob{}, false, err
	}
	return job, ok, nil
}

func (s *Store) GetCronJob(ctx context.Context, id string) (CronJob, bool, error) {
	return s.GetReminderJob(ctx, id)
}

func (s *Store) ListReminderJobs(ctx context.Context, activeOnly bool) ([]ReminderJob, error) {
	query := `
SELECT id, schedule, prompt, transport, session_key, active, safe, auto_prefetch, reminder_mode, reminder_spec_json, once_fire_at, last_run_at, next_run_at, last_error, created_at, updated_at
FROM cron_jobs
`
	if activeOnly {
		query += ` WHERE active = 1`
	}
	query += ` ORDER BY id ASC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list reminder jobs: %w", err)
	}
	defer rows.Close()
	out := []ReminderJob{}
	for rows.Next() {
		var (
			job                    ReminderJob
			active, safe, prefetch int
			onceFireAtRaw          sql.NullString
			lastRunRaw, nextRaw    sql.NullString
			createdRaw, updatedRaw string
		)
		if err := rows.Scan(&job.ID, &job.Schedule, &job.Prompt, &job.Transport, &job.SessionKey, &active, &safe, &prefetch, &job.ReminderMode, &job.ReminderSpecJSON, &onceFireAtRaw, &lastRunRaw, &nextRaw, &job.LastError, &createdRaw, &updatedRaw); err != nil {
			return nil, fmt.Errorf("scan reminder job: %w", err)
		}
		job.Active = active == 1
		job.Safe = safe == 1
		job.AutoPrefetch = prefetch == 1
		job.OnceFireAt = parseNullTime(onceFireAtRaw)
		job.LastRunAt = parseNullTime(lastRunRaw)
		job.NextRunAt = parseNullTime(nextRaw)
		job.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdRaw)
		job.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedRaw)
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reminder jobs: %w", err)
	}
	return out, nil
}

func (s *Store) ListCronJobs(ctx context.Context, activeOnly bool) ([]CronJob, error) {
	return s.ListReminderJobs(ctx, activeOnly)
}

func (s *Store) DeleteReminderJob(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cron_jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete reminder job: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("reminder job %s not found", id)
	}
	return nil
}

func (s *Store) DeleteCronJob(ctx context.Context, id string) error {
	return s.DeleteReminderJob(ctx, id)
}

func (s *Store) UpdateReminderRun(ctx context.Context, id string, lastRun, nextRun *time.Time, lastError string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE cron_jobs
SET last_run_at = ?, next_run_at = ?, last_error = ?, updated_at = ?
WHERE id = ?
`, nullableTimeString(lastRun), nullableTimeString(nextRun), lastError, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("update reminder job run: %w", err)
	}
	return nil
}

func (s *Store) UpdateCronRun(ctx context.Context, id string, lastRun, nextRun *time.Time, lastError string) error {
	return s.UpdateReminderRun(ctx, id, lastRun, nextRun, lastError)
}

func (s *Store) SetReminderActive(ctx context.Context, id string, active bool) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE cron_jobs
SET active = ?, updated_at = ?
WHERE id = ?
`, boolToInt(active), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("set reminder job active: %w", err)
	}
	return nil
}

func scanReminderJob(row *sql.Row) (ReminderJob, bool, error) {
	var (
		job                    ReminderJob
		active, safe, prefetch int
		onceFireAtRaw          sql.NullString
		lastRunRaw, nextRaw    sql.NullString
		createdRaw, updatedRaw string
	)
	err := row.Scan(&job.ID, &job.Schedule, &job.Prompt, &job.Transport, &job.SessionKey, &active, &safe, &prefetch, &job.ReminderMode, &job.ReminderSpecJSON, &onceFireAtRaw, &lastRunRaw, &nextRaw, &job.LastError, &createdRaw, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ReminderJob{}, false, nil
	}
	if err != nil {
		return ReminderJob{}, false, fmt.Errorf("scan reminder job: %w", err)
	}
	job.Active = active == 1
	job.Safe = safe == 1
	job.AutoPrefetch = prefetch == 1
	job.OnceFireAt = parseNullTime(onceFireAtRaw)
	job.LastRunAt = parseNullTime(lastRunRaw)
	job.NextRunAt = parseNullTime(nextRaw)
	job.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdRaw)
	job.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedRaw)
	return job, true, nil
}

func scanCronJob(row *sql.Row) (CronJob, bool, error) {
	return scanReminderJob(row)
}

func (s *Store) ListReminderPrefetchCommands(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT command FROM cron_prefetch_commands WHERE job_id = ? ORDER BY command ASC`, jobID)
	if err != nil {
		return nil, fmt.Errorf("list reminder prefetch commands: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var cmd string
		if err := rows.Scan(&cmd); err != nil {
			return nil, fmt.Errorf("scan reminder prefetch command: %w", err)
		}
		out = append(out, cmd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reminder prefetch commands: %w", err)
	}
	return out, nil
}

func (s *Store) ListCronPrefetchCommands(ctx context.Context, jobID string) ([]string, error) {
	return s.ListReminderPrefetchCommands(ctx, jobID)
}

func (s *Store) UpsertReminderPrefetchCommands(ctx context.Context, jobID string, commands []string) error {
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
			return fmt.Errorf("upsert reminder prefetch command: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) UpsertCronPrefetchCommands(ctx context.Context, jobID string, commands []string) error {
	return s.UpsertReminderPrefetchCommands(ctx, jobID, commands)
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
