package session

import (
	"encoding/json"
	"time"
)

const (
	MaxSourceBytes      = 64 << 20
	MaxRecordBytes      = 16 << 20
	MaxTitleBytes       = 200
	MaxDescriptionBytes = 4 << 10
	MaxTags             = 20
	MaxTagBytes         = 64
	MaxUploaderKeyBytes = 256
)

type EventKind string

const (
	EventUser       EventKind = "user"
	EventAssistant  EventKind = "assistant"
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventFileChange EventKind = "file_change"
	EventCommit     EventKind = "commit"
	EventError      EventKind = "error"
	EventRaw        EventKind = "raw"
)

type Event struct {
	ID       string          `json:"id"`
	ParentID string          `json:"parent_id,omitempty"`
	AgentID  string          `json:"agent_id,omitempty"`
	Kind     EventKind       `json:"kind"`
	Time     time.Time       `json:"time,omitempty"`
	Text     string          `json:"text,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Output   json.RawMessage `json:"output,omitempty"`
	RawType  string          `json:"raw_type,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

type Completion struct {
	Terminal       bool      `json:"terminal"`
	TerminalReason string    `json:"terminal_reason,omitempty"`
	LastEventAt    time.Time `json:"last_event_at,omitempty"`
}

type Session struct {
	SchemaVersion     int        `json:"schema_version"`
	ID                string     `json:"id"`
	Provider          string     `json:"provider"`
	ProviderSessionID string     `json:"provider_session_id,omitempty"`
	Project           string     `json:"project,omitempty"`
	WorkingDirectory  string     `json:"working_directory,omitempty"`
	StartedAt         time.Time  `json:"started_at,omitempty"`
	EndedAt           time.Time  `json:"ended_at,omitempty"`
	Events            []Event    `json:"events"`
	Completion        Completion `json:"completion"`
}

type Directory struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
}

type SourceFacts struct {
	ObservedModTime     time.Time `json:"observed_mod_time,omitempty"`
	ObservedSize        int64     `json:"observed_size"`
	QuietPeriodVerified bool      `json:"quiet_period_verified"`
}

type Metadata struct {
	ID                      string    `json:"id"`
	ContentID               string    `json:"content_id"`
	Provider                string    `json:"provider"`
	ProviderSessionID       string    `json:"provider_session_id,omitempty"`
	Title                   string    `json:"title,omitempty"`
	Description             string    `json:"description,omitempty"`
	Tags                    []string  `json:"tags,omitempty"`
	Project                 string    `json:"project,omitempty"`
	WorkingDirectory        string    `json:"working_directory,omitempty"`
	StartedAt               time.Time `json:"started_at,omitempty"`
	EndedAt                 time.Time `json:"ended_at,omitempty"`
	UploaderKey             string    `json:"uploader_key,omitempty"`
	UploadedAt              time.Time `json:"uploaded_at,omitempty"`
	Destination             Directory `json:"destination"`
	SourceChecksum          string    `json:"source_checksum,omitempty"`
	ParserVersion           int       `json:"parser_version,omitempty"`
	NormalizedSchemaVersion int       `json:"normalized_schema_version,omitempty"`
	Revision                string    `json:"revision,omitempty"`
}

type Package struct {
	ID          string      `json:"id"`
	ContentID   string      `json:"content_id"`
	Session     Session     `json:"session"`
	Metadata    Metadata    `json:"metadata"`
	Source      []byte      `json:"-"`
	Normalized  []byte      `json:"-"`
	SourceFacts SourceFacts `json:"source_facts"`
}
