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
