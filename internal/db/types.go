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

type Plugin struct {
	ID            string
	Name          string
	Version       string
	Source        string
	ResolvedRef   string
	Checksum      string
	InstallPath   string
	Permissions   string
	Enabled       bool
	InstalledAt   time.Time
	LastUpdatedAt time.Time
}

type PluginTool struct {
	PluginID    string
	ToolName    string
	Description string
	SchemaJSON  string
	TimeoutSec  int
	Enabled     bool
	UpdatedAt   time.Time
}

type CronJob struct {
	ID           string
	Schedule     string
	Prompt       string
	Transport    string
	SessionKey   string
	Active       bool
	Safe         bool
	AutoPrefetch bool
	LastRunAt    *time.Time
	NextRunAt    *time.Time
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
