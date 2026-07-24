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
	MaxFamilySources    = 256
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
	ID           string          `json:"id"`
	ParentID     string          `json:"parent_id,omitempty"`
	AgentID      string          `json:"agent_id,omitempty"`
	Kind         EventKind       `json:"kind"`
	Time         time.Time       `json:"time,omitempty"`
	Text         string          `json:"text,omitempty"`
	ToolName     string          `json:"tool_name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Output       json.RawMessage `json:"output,omitempty"`
	ResultStatus string          `json:"result_status,omitempty"`
	RawType      string          `json:"raw_type,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

// TokenUsage is provider-normalized token evidence for one completed usage
// snapshot. ReasoningOutput is a subset of provider output and is therefore
// intentionally excluded from Total.
type TokenUsage struct {
	Input           int64 `json:"input"`
	Output          int64 `json:"output"`
	CacheRead       int64 `json:"cache_read,omitempty"`
	CacheWrite      int64 `json:"cache_write,omitempty"`
	ReasoningOutput int64 `json:"reasoning_output,omitempty"`
}

func (u TokenUsage) Total() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheWrite
}

// UsageSample identifies one provider usage snapshot independently of the
// visible event stream, whose records may be repeated or partial.
type UsageSample struct {
	ID     string     `json:"id"`
	Time   time.Time  `json:"time"`
	Model  string     `json:"model,omitempty"`
	Tokens TokenUsage `json:"tokens"`
}

type Completion struct {
	Terminal       bool      `json:"terminal"`
	TerminalReason string    `json:"terminal_reason,omitempty"`
	LastEventAt    time.Time `json:"last_event_at,omitempty"`
}

// ProjectRef is the persisted, opaque identity of the project that produced a
// family. CanonicalRoot deliberately belongs only to ProjectScope.
type ProjectRef struct {
	Kind        string `json:"kind"`
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
}

type ProjectScope struct {
	Ref           ProjectRef
	CanonicalRoot string
}

type FamilyCompletion struct {
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
	LastEventAt time.Time `json:"last_event_at,omitempty"`
}

type ChildSession struct {
	AgentID          string  `json:"agent_id"`
	ParentSessionID  string  `json:"parent_session_id,omitempty"`
	ParentToolCallID string  `json:"parent_tool_call_id,omitempty"`
	AgentType        string  `json:"agent_type,omitempty"`
	Description      string  `json:"description,omitempty"`
	Attached         bool    `json:"attached"`
	Session          Session `json:"session"`
}

type SessionFamily struct {
	SchemaVersion     int              `json:"schema_version"`
	ID                string           `json:"id"`
	Provider          string           `json:"provider"`
	ProviderSessionID string           `json:"provider_session_id"`
	Project           ProjectRef       `json:"project"`
	Main              Session          `json:"main"`
	Children          []ChildSession   `json:"children"`
	StartedAt         time.Time        `json:"started_at,omitempty"`
	EndedAt           time.Time        `json:"ended_at,omitempty"`
	Completion        FamilyCompletion `json:"completion"`
}

type Session struct {
	SchemaVersion     int           `json:"schema_version"`
	ID                string        `json:"id"`
	Provider          string        `json:"provider"`
	ProviderSessionID string        `json:"provider_session_id,omitempty"`
	Project           string        `json:"project,omitempty"`
	WorkingDirectory  string        `json:"working_directory,omitempty"`
	StartedAt         time.Time     `json:"started_at,omitempty"`
	EndedAt           time.Time     `json:"ended_at,omitempty"`
	Events            []Event       `json:"events"`
	Usage             []UsageSample `json:"usage,omitempty"`
	Completion        Completion    `json:"completion"`
	Origin            SessionOrigin `json:"origin,omitempty"`
}

// SessionOrigin records provider-supplied relationship evidence for a nested
// session. It is optional for backward compatibility with schema-v2 packages
// created before session relationships were normalized.
type SessionOrigin struct {
	Kind            string `json:"kind,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	AgentPath       string `json:"agent_path,omitempty"`
	AgentName       string `json:"agent_name,omitempty"`
	AgentRole       string `json:"agent_role,omitempty"`
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

type SourceEntry struct {
	Role     string `json:"role"`
	AgentID  string `json:"agent_id,omitempty"`
	Checksum string `json:"checksum"`
	Bytes    int64  `json:"bytes"`
	Name     string `json:"name"`
}

type SourceManifest struct {
	SchemaVersion int           `json:"schema_version"`
	Provider      string        `json:"provider"`
	SessionID     string        `json:"session_id"`
	Sources       []SourceEntry `json:"sources"`
}

type SourceBlob struct {
	Entry SourceEntry `json:"entry"`
	Bytes []byte      `json:"-"`
}

type SourceFactEntry struct {
	Role    string      `json:"role"`
	AgentID string      `json:"agent_id,omitempty"`
	Facts   SourceFacts `json:"facts"`
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
	// Family fields are used by schema-v2 packages. The singular fields remain
	// for schema-v1 compatibility and one-source adapters.
	SchemaVersion  int               `json:"schema_version,omitempty"`
	Family         SessionFamily     `json:"family,omitempty"`
	SourceManifest SourceManifest    `json:"source_manifest,omitempty"`
	Sources        []SourceBlob      `json:"-"`
	SourceFactsSet []SourceFactEntry `json:"source_facts_set,omitempty"`
}
