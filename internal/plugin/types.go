package plugin

import "encoding/json"

type Manifest struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Version     string                 `json:"version"`
	APIVersion  string                 `json:"apiVersion"`
	Description string                 `json:"description,omitempty"`
	Entrypoint  Entrypoint             `json:"entrypoint"`
	Protocol    Protocol               `json:"protocol"`
	Permissions map[string]interface{} `json:"permissions,omitempty"`
}

type Entrypoint struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type Protocol struct {
	JSONRPC   string `json:"jsonrpc"`
	Transport string `json:"transport"`
	Framing   string `json:"framing"`
}

type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	TimeoutSec  int             `json:"timeout_seconds,omitempty"`
}

type LockFile struct {
	Version int         `json:"version"`
	Plugins []LockEntry `json:"plugins"`
}

type LockEntry struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Source      string `json:"source"`
	ResolvedRef string `json:"resolved_ref"`
	Checksum    string `json:"checksum"`
	InstalledAt string `json:"installed_at"`
	Path        string `json:"path"`
}
