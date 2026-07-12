package session

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	logicalIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	providerPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	slugPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	tagPattern       = regexp.MustCompile(`^[a-z0-9_-]+$`)
)

func NormalizeTags(tags []string) ([]string, error) {
	if len(tags) > MaxTags {
		return nil, fmt.Errorf("tags: maximum is %d", MaxTags)
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
	}
	return result, nil
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
	return nil
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
	if err := validateID("package ID", m.ID); err != nil {
		return err
	}
	if err := validateID("content ID", m.ContentID); err != nil {
		return err
	}
	if !providerPattern.MatchString(m.Provider) {
		return fmt.Errorf("invalid provider")
	}
	if len(m.Title) > MaxTitleBytes {
		return fmt.Errorf("title exceeds %d bytes", MaxTitleBytes)
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
	if p.ID != "" && p.ID != p.Metadata.ID {
		return fmt.Errorf("package ID mismatch")
	}
	if p.ContentID != "" && p.ContentID != p.Metadata.ContentID {
		return fmt.Errorf("content ID mismatch")
	}
	if p.Session.ID != p.Metadata.ID || p.Session.Provider != p.Metadata.Provider {
		return fmt.Errorf("session metadata mismatch")
	}
	if len(p.Normalized) != 0 && !json.Valid(p.Normalized) {
		return fmt.Errorf("normalized document is not valid JSON")
	}
	return nil
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
