package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type codexParser struct{}

func (codexParser) Provider() string { return "codex" }
func (codexParser) Detect(first json.RawMessage) bool {
	var e envelope
	return json.Unmarshal(first, &e) == nil && e.Type == "session_meta"
}

type codexPayload struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	CWD       string          `json:"cwd"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	CallID    string          `json:"call_id"`
	Content   []codexContent  `json:"content"`
}
type codexContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (codexParser) Parse(ctx context.Context, lines []json.RawMessage) (session.Session, error) {
	got := session.Session{SchemaVersion: 1, Provider: "codex"}
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
			return session.Session{}, fmt.Errorf("decode Codex line %d: %w", lineNumber, err)
		}
		when, err := parseTime(e.Timestamp)
		if err != nil {
			return session.Session{}, fmt.Errorf("Codex line %d timestamp: %w", lineNumber, err)
		}
		observe(&got, when)
		var p codexPayload
		if len(e.Payload) != 0 {
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return session.Session{}, fmt.Errorf("decode Codex payload at line %d: %w", lineNumber, err)
			}
		}
		if e.Type == "session_meta" {
			if got.ProviderSessionID == "" {
				got.ProviderSessionID, got.ID, got.WorkingDirectory = p.ID, p.ID, p.CWD
			}
			continue
		}
		if e.Type == "event_msg" && p.Type == "task_complete" {
			got.Completion.Terminal, got.Completion.TerminalReason = true, p.Type
			continue
		}
		if event, ok := mapCodexResponse(p, lineNumber, when); e.Type == "response_item" && ok {
			got.Events = append(got.Events, event)
			continue
		}
		got.Events = append(got.Events, rawEvent(eventID(p.ID, lineNumber), "", e.Type, when, line))
	}
	if got.ID == "" {
		got.ID = "session-line-1"
	}
	return got, nil
}

func mapCodexResponse(p codexPayload, line int, when time.Time) (session.Event, bool) {
	id := eventID(p.ID, line)
	switch p.Type {
	case "message":
		kind := session.EventAssistant
		if p.Role == "user" {
			kind = session.EventUser
		} else if p.Role != "assistant" {
			return session.Event{}, false
		}
		text := ""
		for _, content := range p.Content {
			if content.Type == "input_text" || content.Type == "output_text" {
				text += content.Text
			} else {
				return session.Event{}, false
			}
		}
		return session.Event{ID: id, Kind: kind, Time: when, Text: text}, true
	case "function_call":
		return session.Event{ID: id, Kind: session.EventToolCall, Time: when, ToolName: p.Name, Input: jsonValue(p.Arguments)}, true
	case "function_call_output":
		return session.Event{ID: id, ParentID: p.CallID, Kind: session.EventToolResult, Time: when, Output: jsonValue(p.Output)}, true
	default:
		return session.Event{}, false
	}
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}
func observe(got *session.Session, when time.Time) {
	if when.IsZero() {
		return
	}
	if got.StartedAt.IsZero() || when.Before(got.StartedAt) {
		got.StartedAt = when
	}
	if got.Completion.LastEventAt.IsZero() || when.After(got.Completion.LastEventAt) {
		got.Completion.LastEventAt = when
	}
}
func eventID(id string, line int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("line-%d", line)
}
func blockFallbackID(line, index int) string {
	return fmt.Sprintf("line-%d-block-%d", line, index+1)
}
func rawEvent(id, parent, typ string, when time.Time, raw json.RawMessage) session.Event {
	if typ == "" {
		typ = "unknown"
	}
	return session.Event{ID: id, ParentID: parent, Kind: session.EventRaw, Time: when, RawType: typ, Raw: append(json.RawMessage(nil), raw...)}
}
func jsonValue(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if json.Valid(raw) {
		return append(json.RawMessage(nil), raw...)
	}
	b, _ := json.Marshal(string(raw))
	return b
}
