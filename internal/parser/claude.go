package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type claudeParser struct{}

func (claudeParser) Provider() string { return "claude" }
func (claudeParser) Detect(first json.RawMessage) bool {
	var e envelope
	if json.Unmarshal(first, &e) != nil {
		return false
	}
	return e.SessionID != "" && (e.Type == "user" || e.Type == "assistant" || e.Type == "system")
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
}

func (claudeParser) Parse(ctx context.Context, lines []json.RawMessage) (session.Session, error) {
	got := session.Session{SchemaVersion: 1, Provider: "claude"}
	for i, line := range lines {
		if line == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return session.Session{}, err
		}
		lineNumber := i + 1
		var e envelope
		if err := json.Unmarshal(line, &e); err != nil {
			return session.Session{}, fmt.Errorf("decode Claude line %d: %w", lineNumber, err)
		}
		when, err := parseTime(e.Timestamp)
		if err != nil {
			return session.Session{}, fmt.Errorf("Claude line %d timestamp: %w", lineNumber, err)
		}
		observe(&got, when)
		if got.ProviderSessionID == "" && e.SessionID != "" {
			got.ProviderSessionID, got.ID = e.SessionID, e.SessionID
		}
		if got.WorkingDirectory == "" {
			got.WorkingDirectory = e.CWD
		}
		if e.Type == "system" && e.Subtype == "turn_duration" {
			got.Completion.Terminal, got.Completion.TerminalReason = true, e.Subtype
			continue
		}
		events, mapped, err := mapClaudeMessage(e, lineNumber, when)
		if err != nil {
			return session.Session{}, err
		}
		if mapped {
			got.Events = append(got.Events, events...)
			continue
		}
		got.Events = append(got.Events, rawEvent(eventID(e.UUID, lineNumber), e.ParentUUID, e.Type, when, line))
	}
	if got.ID == "" {
		got.ID = "session-line-1"
	}
	return got, nil
}

func mapClaudeMessage(e envelope, line int, when time.Time) ([]session.Event, bool, error) {
	if e.Type != "user" && e.Type != "assistant" {
		return nil, false, nil
	}
	var message claudeMessage
	if err := json.Unmarshal(e.Message, &message); err != nil {
		return nil, false, fmt.Errorf("decode Claude message at line %d: %w", line, err)
	}
	var text string
	if err := json.Unmarshal(message.Content, &text); err == nil {
		kind := session.EventUser
		if e.Type == "assistant" {
			kind = session.EventAssistant
		}
		return []session.Event{{ID: eventID(e.UUID, line), ParentID: e.ParentUUID, Kind: kind, Time: when, Text: text}}, true, nil
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, false, fmt.Errorf("decode Claude content at line %d: %w", line, err)
	}
	var events []session.Event
	for blockIndex, block := range blocks {
		switch block.Type {
		case "text":
			kind := session.EventUser
			if e.Type == "assistant" {
				kind = session.EventAssistant
			}
			events = append(events, session.Event{ID: indexedID(eventID(e.UUID, line), blockIndex), ParentID: e.ParentUUID, Kind: kind, Time: when, Text: block.Text})
		case "tool_use":
			events = append(events, session.Event{ID: eventID(block.ID, line), ParentID: e.UUID, Kind: session.EventToolCall, Time: when, ToolName: block.Name, Input: block.Input})
		case "tool_result":
			events = append(events, session.Event{ID: indexedID(eventID(e.UUID, line), blockIndex), ParentID: block.ToolUseID, Kind: session.EventToolResult, Time: when, Output: jsonValue(block.Content)})
		default:
			return nil, false, nil
		}
	}
	return events, true, nil
}
