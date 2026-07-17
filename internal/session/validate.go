package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const maxRawTagInputs = 1000

var (
	logicalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	providerPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	slugPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	tagPattern       = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

func NormalizeTags(tags []string) ([]string, error) {
	if len(tags) > maxRawTagInputs {
		return nil, fmt.Errorf("tags: too many inputs")
	}
	result := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, value := range tags {
		tag := strings.ToLower(strings.TrimSpace(value))
		if len(tag) == 0 || len(tag) > MaxTagBytes || !tagPattern.MatchString(tag) {
			return nil, fmt.Errorf("invalid tag %q", value)
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		result = append(result, tag)
		if len(result) > MaxTags {
			return nil, fmt.Errorf("tags: maximum is %d", MaxTags)
		}
	}
	return result, nil
}

// NormalizeUploaderKey returns a stable provider-neutral identity key suitable
// for comparisons and logical ownership checks.
func NormalizeUploaderKey(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid uploader key")
	}
	key := strings.ToLower(strings.TrimSpace(value))
	if key == "" || len(key) > MaxUploaderKeyBytes {
		return "", fmt.Errorf("invalid uploader key")
	}
	for _, r := range key {
		if r <= 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return "", fmt.Errorf("invalid uploader key")
		}
	}
	return key, nil
}

func Validate(s Session) error {
	if s.SchemaVersion <= 0 {
		return fmt.Errorf("schema version must be positive")
	}
	if err := validateID("session ID", s.ID); err != nil {
		return err
	}
	if !providerPattern.MatchString(s.Provider) {
		return fmt.Errorf("invalid provider")
	}
	if s.ProviderSessionID != "" {
		if err := validateID("provider session ID", s.ProviderSessionID); err != nil {
			return err
		}
	}
	if !s.StartedAt.IsZero() && !s.EndedAt.IsZero() && s.EndedAt.Before(s.StartedAt) {
		return fmt.Errorf("end timestamp precedes start timestamp")
	}
	if s.Completion.Terminal && strings.TrimSpace(s.Completion.TerminalReason) == "" {
		return fmt.Errorf("terminal completion requires a reason")
	}
	if !s.Completion.LastEventAt.IsZero() && !s.StartedAt.IsZero() && s.Completion.LastEventAt.Before(s.StartedAt) {
		return fmt.Errorf("last event timestamp precedes start timestamp")
	}
	for i, event := range s.Events {
		if err := validateEvent(event); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
	}
	if err := validateSessionOrigin(s.Origin); err != nil {
		return err
	}
	return nil
}

func ValidateFamily(f SessionFamily) error {
	if f.SchemaVersion != 2 {
		return fmt.Errorf("family schema version must be 2")
	}
	if err := validateID("family ID", f.ID); err != nil {
		return err
	}
	if !providerPattern.MatchString(f.Provider) || f.ProviderSessionID == "" || f.ProviderSessionID != f.ID {
		return fmt.Errorf("invalid family provider identity")
	}
	if f.Project.Kind != "git_worktree" && f.Project.Kind != "directory" && f.Project.Kind != "unresolved" {
		return fmt.Errorf("invalid project kind")
	}
	if !hasManagedID(f.Project.Key, "p_") || strings.TrimSpace(f.Project.DisplayName) == "" {
		return fmt.Errorf("invalid project reference")
	}
	if err := Validate(f.Main); err != nil {
		return fmt.Errorf("main: %w", err)
	}
	if f.Main.Provider != f.Provider || f.Main.ProviderSessionID != "" && f.Main.ProviderSessionID != f.ProviderSessionID {
		return fmt.Errorf("main provider identity mismatch")
	}
	if f.Main.Origin != (SessionOrigin{}) {
		return fmt.Errorf("main session origin must be empty")
	}
	if len(f.Children)+1 > MaxFamilySources {
		return fmt.Errorf("family exceeds %d sources", MaxFamilySources)
	}
	members := []Session{f.Main}
	seen := make(map[string]struct{}, len(f.Children))
	for i, child := range f.Children {
		if err := validateID("child agent ID", child.AgentID); err != nil {
			return fmt.Errorf("child %d: %w", i, err)
		}
		if _, ok := seen[child.AgentID]; ok {
			return fmt.Errorf("duplicate child agent ID")
		}
		seen[child.AgentID] = struct{}{}
		if child.Attached != (child.ParentToolCallID != "") {
			return fmt.Errorf("child attachment mismatch")
		}
		if err := Validate(child.Session); err != nil {
			return fmt.Errorf("child %d: %w", i, err)
		}
		if child.Session.Provider != f.Provider {
			return fmt.Errorf("child provider identity mismatch")
		}
		if f.Provider == "claude" && child.Session.ProviderSessionID != f.ProviderSessionID {
			return fmt.Errorf("child provider identity mismatch")
		}
		members = append(members, child.Session)
	}
	familyMembers, err := familyMemberByID(f)
	if err != nil {
		return err
	}
	if err := validateFamilyParents(f, familyMembers); err != nil {
		return err
	}
	start, end, last := derivedFamilyTimes(members)
	if !f.StartedAt.Equal(start) || !f.EndedAt.Equal(end) || !f.Completion.LastEventAt.Equal(last) {
		return fmt.Errorf("family timestamps are not derived")
	}
	allTerminal := true
	for _, member := range members {
		allTerminal = allTerminal && member.Completion.Terminal
	}
	if allTerminal {
		if f.Completion.Status != "provider_terminal" || f.Completion.Reason != "all_members_terminal" {
			return fmt.Errorf("family completion is not derived")
		}
	} else if f.Completion.Status != "local_quiet" && f.Completion.Status != "incomplete" {
		return fmt.Errorf("invalid family completion")
	}
	return nil
}

// familyMemberByID returns the sessions that may be referenced as parents.
// Claude records all children against the main provider session, whereas Codex
// descendants each have their own provider session identity.
func familyMemberByID(f SessionFamily) (map[string]Session, error) {
	members := map[string]Session{f.Main.ID: f.Main}
	if f.Provider != "codex" {
		return members, nil
	}
	for _, child := range f.Children {
		if child.Session.ID == f.Main.ID {
			return nil, fmt.Errorf("Codex child reuses root session ID")
		}
		if _, exists := members[child.Session.ID]; exists {
			return nil, fmt.Errorf("duplicate member session ID")
		}
		members[child.Session.ID] = child.Session
	}
	return members, nil
}

func validateFamilyParents(f SessionFamily, members map[string]Session) error {
	parentOf := make(map[string]string, len(f.Children))
	for _, child := range f.Children {
		parent := child.ParentSessionID
		if parent == "" {
			parent = f.Main.ID
		}
		parentSession, exists := members[parent]
		if !exists {
			return fmt.Errorf("unknown parent session %q", parent)
		}
		if f.Provider == "claude" {
			if parent != f.Main.ID {
				return errors.New("Claude child parent is not the family root")
			}
			if child.Session.Origin != (SessionOrigin{}) {
				return errors.New("Claude child origin must be empty")
			}
		} else if f.Provider == "codex" {
			if child.AgentID != child.Session.ID || child.Session.ProviderSessionID != child.Session.ID {
				return errors.New("Codex child identity mismatch")
			}
			if child.Session.Origin.ParentSessionID != parent || child.AgentType != child.Session.Origin.Kind {
				return errors.New("Codex child origin mismatch")
			}
			parentOf[child.Session.ID] = parent
		} else if parent != f.Main.ID {
			return errors.New("child parent is not the family root")
		}
		if err := validateParentToolCall(child, parentSession); err != nil {
			return err
		}
	}
	for child := range parentOf {
		seen := map[string]bool{}
		for current := child; current != f.Main.ID; current = parentOf[current] {
			if current == "" || seen[current] {
				return fmt.Errorf("family parent graph is cyclic or unreachable")
			}
			seen[current] = true
		}
	}
	return nil
}

func validateParentToolCall(child ChildSession, parent Session) error {
	if !child.Attached {
		return nil
	}
	matches := 0
	for _, event := range parent.Events {
		if event.ID == child.ParentToolCallID && event.Kind == EventToolCall && (event.ToolName == "Agent" || event.ToolName == "Task") {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("attached child parent tool call must identify one Agent or Task call")
	}
	return nil
}

func validateSessionOrigin(origin SessionOrigin) error {
	if origin == (SessionOrigin{}) {
		return nil
	}
	if origin.Kind != "thread_spawn" && origin.Kind != "guardian" {
		return fmt.Errorf("invalid session origin kind")
	}
	if err := validateID("origin parent session ID", origin.ParentSessionID); err != nil {
		return err
	}
	for label, value := range map[string]string{
		"origin agent path": origin.AgentPath,
		"origin agent name": origin.AgentName,
		"origin agent role": origin.AgentRole,
	} {
		if !utf8.ValidString(value) || len(value) > MaxDescriptionBytes {
			return fmt.Errorf("invalid %s", label)
		}
	}
	return nil
}

func derivedFamilyTimes(members []Session) (time.Time, time.Time, time.Time) {
	var start, end, last time.Time
	for _, member := range members {
		if !member.StartedAt.IsZero() && (start.IsZero() || member.StartedAt.Before(start)) {
			start = member.StartedAt
		}
		effectiveEnd := member.EndedAt
		if effectiveEnd.IsZero() {
			effectiveEnd = member.Completion.LastEventAt
		}
		if !effectiveEnd.IsZero() && (end.IsZero() || effectiveEnd.After(end)) {
			end = effectiveEnd
		}
		if !member.Completion.LastEventAt.IsZero() && (last.IsZero() || member.Completion.LastEventAt.After(last)) {
			last = member.Completion.LastEventAt
		}
	}
	return start, end, last
}

func validateEvent(event Event) error {
	if err := validateID("event ID", event.ID); err != nil {
		return err
	}
	for label, id := range map[string]string{"parent ID": event.ParentID, "agent ID": event.AgentID} {
		if id != "" {
			if err := validateID(label, id); err != nil {
				return err
			}
		}
	}
	switch event.Kind {
	case EventUser, EventAssistant, EventToolCall, EventToolResult, EventFileChange, EventCommit, EventError, EventRaw:
	default:
		return fmt.Errorf("invalid event kind %q", event.Kind)
	}
	for label, raw := range map[string]json.RawMessage{"input": event.Input, "output": event.Output} {
		if len(raw) > MaxRecordBytes {
			return fmt.Errorf("%s exceeds %d bytes", label, MaxRecordBytes)
		}
		if len(raw) != 0 && !json.Valid(raw) {
			return fmt.Errorf("%s is not valid JSON", label)
		}
	}
	if event.Kind == EventRaw {
		if strings.TrimSpace(event.RawType) == "" {
			return fmt.Errorf("raw event type is required")
		}
		if len(event.Raw) > MaxRecordBytes {
			return fmt.Errorf("raw event payload exceeds %d bytes", MaxRecordBytes)
		}
		if len(event.Raw) == 0 || !json.Valid(event.Raw) {
			return fmt.Errorf("raw event payload must be valid JSON")
		}
	} else if len(event.Raw) != 0 || event.RawType != "" {
		return fmt.Errorf("raw fields require raw event kind")
	}
	return nil
}

func ValidateMetadata(m Metadata) error {
	if !hasManagedID(m.ID, "s_") {
		return fmt.Errorf("invalid package ID")
	}
	if !hasManagedID(m.ContentID, "c_") {
		return fmt.Errorf("invalid content ID")
	}
	if !providerPattern.MatchString(m.Provider) {
		return fmt.Errorf("invalid provider")
	}
	if !utf8.ValidString(m.Title) {
		return fmt.Errorf("title is not valid UTF-8")
	}
	if len(m.Title) > MaxTitleBytes {
		return fmt.Errorf("title exceeds %d bytes", MaxTitleBytes)
	}
	if !utf8.ValidString(m.Description) {
		return fmt.Errorf("description is not valid UTF-8")
	}
	if len(m.Description) > MaxDescriptionBytes {
		return fmt.Errorf("description exceeds %d bytes", MaxDescriptionBytes)
	}
	normalized, err := NormalizeTags(m.Tags)
	if err != nil {
		return err
	}
	if len(normalized) != len(m.Tags) {
		return fmt.Errorf("tags must be normalized and unique")
	}
	for i := range normalized {
		if normalized[i] != m.Tags[i] {
			return fmt.Errorf("tags must be normalized")
		}
	}
	if err := ValidateDirectory(m.Destination); err != nil {
		return err
	}
	normalizedUploader, err := NormalizeUploaderKey(m.UploaderKey)
	if err != nil {
		return err
	}
	if normalizedUploader != m.UploaderKey {
		return fmt.Errorf("uploader key must be normalized")
	}
	if !m.StartedAt.IsZero() && !m.EndedAt.IsZero() && m.EndedAt.Before(m.StartedAt) {
		return fmt.Errorf("end timestamp precedes start timestamp")
	}
	if m.SourceChecksum != "" && !isLowerHex(m.SourceChecksum, 64) {
		return fmt.Errorf("invalid source checksum")
	}
	if m.ParserVersion < 0 || m.NormalizedSchemaVersion < 0 {
		return fmt.Errorf("versions cannot be negative")
	}
	return nil
}

func ValidateDirectory(d Directory) error {
	if d.Kind != "users" && d.Kind != "projects" {
		return fmt.Errorf("invalid directory kind %q", d.Kind)
	}
	if !slugPattern.MatchString(d.Slug) || len(d.Slug) > 64 {
		return fmt.Errorf("invalid directory slug")
	}
	return nil
}

func ValidateSourceFacts(f SourceFacts) error {
	if f.ObservedSize < 0 || f.ObservedSize > MaxSourceBytes {
		return fmt.Errorf("invalid observed source size")
	}
	return nil
}

func ValidatePackage(p Package) error {
	if len(p.Source) > MaxSourceBytes {
		return fmt.Errorf("source exceeds %d bytes", MaxSourceBytes)
	}
	if err := Validate(p.Session); err != nil {
		return err
	}
	if err := ValidateMetadata(p.Metadata); err != nil {
		return err
	}
	if err := ValidateSourceFacts(p.SourceFacts); err != nil {
		return err
	}
	if !hasManagedID(p.ID, "s_") || p.ID != p.Metadata.ID {
		return fmt.Errorf("package ID mismatch")
	}
	if !hasManagedID(p.ContentID, "c_") || p.ContentID != p.Metadata.ContentID {
		return fmt.Errorf("content ID mismatch")
	}
	if p.Session.Provider != p.Metadata.Provider {
		return fmt.Errorf("session metadata mismatch")
	}
	if len(p.Normalized) != 0 && !json.Valid(p.Normalized) {
		return fmt.Errorf("normalized document is not valid JSON")
	}
	return nil
}

func hasManagedID(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && isLowerHex(strings.TrimPrefix(value, prefix), 64)
}

func validateID(label, value string) error {
	if len(value) > 128 || !logicalIDPattern.MatchString(value) {
		return fmt.Errorf("invalid %s", label)
	}
	return nil
}
func isLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
