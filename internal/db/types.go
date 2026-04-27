package db

import "time"

type Session struct {
	ID               string
	Transport        string
	SessionKey       string
	ModelOverride    string
	IsActive         bool
	TotalPromptToken int
	TotalEvalToken   int
	CompactionCount  int
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Message struct {
	ID              int64
	SessionID       string
	Seq             int
	Role            string
	Content         string
	Thinking        string
	ToolName        string
	ToolCallID      string
	ToolArgsJSON    string
	ToolCallsJSON   string
	PromptEvalCount int
	EvalCount       int
	Archived        bool
	CreatedAt       time.Time
}

type Compaction struct {
	ID                int64
	SessionID         string
	Summary           string
	FirstKeptMessage  int64
	ArchivedBeforeSeq int
	CreatedAt         time.Time
}

type ReminderJob struct {
	ID               string
	Schedule         string
	Prompt           string
	Transport        string
	SessionKey       string
	Active           bool
	Safe             bool
	AutoPrefetch     bool
	ReminderMode     string
	ReminderSpecJSON string
	OnceFireAt       *time.Time
	LastRunAt        *time.Time
	NextRunAt        *time.Time
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type CronJob = ReminderJob

type SubagentTask struct {
	ID           string
	Kind         string
	Status       string
	Title        string
	Prompt       string
	Transport    string
	SessionKey   string
	Repo         string
	PRNumber     int
	PRURL        string
	BaseRef      string
	HeadRef      string
	WorktreePath string
	ResultPath   string
	StdoutPath   string
	StderrPath   string
	MetadataJSON string
	PID          int
	ExitCode     *int
	Error        string
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	UpdatedAt    time.Time
}

type SubagentTaskFilter struct {
	Status     string
	Kind       string
	Repo       string
	Transport  string
	SessionKey string
	Limit      int
}
