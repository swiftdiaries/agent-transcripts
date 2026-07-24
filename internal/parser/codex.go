package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Role           string          `json:"role"`
	CWD            string          `json:"cwd"`
	ParentThreadID string          `json:"parent_thread_id"`
	ThreadSource   string          `json:"thread_source"`
	Source         json.RawMessage `json:"source"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	Input          json.RawMessage `json:"input"`
	Output         json.RawMessage `json:"output"`
	CallID         string          `json:"call_id"`
	Content        []codexContent  `json:"content"`
}

type codexSubagentSource struct {
	ThreadSpawn *struct {
		ParentThreadID string `json:"parent_thread_id"`
		AgentPath      string `json:"agent_path"`
		AgentNickname  string `json:"agent_nickname"`
		AgentRole      string `json:"agent_role"`
	} `json:"thread_spawn,omitempty"`
	Other string `json:"other,omitempty"`
}
type codexContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type codexTurnContext struct {
	TurnID string `json:"turn_id"`
	Model  string `json:"model"`
}

type codexTokenCount struct {
	Type string `json:"type"`
	Info struct {
		Last *struct {
			Input           int64 `json:"input_tokens"`
			CachedInput     int64 `json:"cached_input_tokens"`
			CacheWriteInput int64 `json:"cache_write_input_tokens"`
			Output          int64 `json:"output_tokens"`
			ReasoningOutput int64 `json:"reasoning_output_tokens"`
		} `json:"last_token_usage"`
	} `json:"info"`
}

func (codexParser) Parse(ctx context.Context, lines []json.RawMessage) (session.Session, error) {
	got := session.Session{SchemaVersion: 1, Provider: "codex"}
	var currentTurnID, currentModel string
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
		if e.Type == "turn_context" {
			var turn codexTurnContext
			if err := json.Unmarshal(e.Payload, &turn); err != nil {
				return session.Session{}, fmt.Errorf("decode Codex turn context at line %d: %w", lineNumber, err)
			}
			currentTurnID, currentModel = turn.TurnID, turn.Model
		}
		if e.Type == "event_msg" {
			var count codexTokenCount
			if err := json.Unmarshal(e.Payload, &count); err != nil {
				return session.Session{}, fmt.Errorf("decode Codex token count at line %d: %w", lineNumber, err)
			}
			if count.Type == "token_count" && count.Info.Last != nil {
				usage := count.Info.Last
				uncached := usage.Input - usage.CachedInput - usage.CacheWriteInput
				if uncached < 0 {
					return session.Session{}, fmt.Errorf("Codex line %d cached input exceeds input tokens", lineNumber)
				}
				got.Usage = append(got.Usage, session.UsageSample{
					ID: fmt.Sprintf("%s-token-%d", currentTurnID, lineNumber), Time: when, Model: currentModel,
					Tokens: session.TokenUsage{Input: uncached, Output: usage.Output, CacheRead: usage.CachedInput, CacheWrite: usage.CacheWriteInput, ReasoningOutput: usage.ReasoningOutput},
				})
			}
		}
		if e.Type == "session_meta" {
			origin, err := codexOrigin(p)
			if err != nil {
				return session.Session{}, fmt.Errorf("Codex session origin at line %d: %w", lineNumber, err)
			}
			if got.ProviderSessionID == "" {
				got.ProviderSessionID, got.ID, got.WorkingDirectory = p.ID, p.ID, p.CWD
				got.Origin = origin
			}
			continue
		}
		if e.Type == "event_msg" && p.Type == "task_complete" {
			got.Completion.Terminal, got.Completion.TerminalReason = true, p.Type
			continue
		}
		if e.Type == "event_msg" && p.Type == "user_message" {
			var event struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(e.Payload, &event) == nil && event.Message != "" {
				// Codex commonly emits the same user turn first as a response item
				// and then as its canonical event message. Preserve the former as
				// diagnostic evidence, but do not create a second visible prompt.
				if n := len(got.Events); n > 0 && got.Events[n-1].Kind == session.EventUser && got.Events[n-1].Text == event.Message {
					paired := got.Events[n-1]
					raw, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []codexContent{{Type: "input_text", Text: paired.Text}}}})
					got.Events[n-1] = rawEvent(paired.ID, "", "paired_response_user", paired.Time, raw)
				}
				got.Events = append(got.Events, session.Event{ID: eventID(p.ID, lineNumber), Kind: session.EventUser, Time: when, Text: event.Message})
				continue
			}
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

func codexOrigin(p codexPayload) (session.SessionOrigin, error) {
	switch p.ThreadSource {
	case "":
		if p.ParentThreadID != "" {
			return session.SessionOrigin{}, errors.New("Codex legacy root has parent evidence")
		}
		return session.SessionOrigin{}, nil
	case "user":
		if p.ParentThreadID != "" {
			return session.SessionOrigin{}, errors.New("Codex user root has parent evidence")
		}
		return session.SessionOrigin{}, nil
	case "subagent":
		if p.ParentThreadID == "" {
			return session.SessionOrigin{}, errors.New("Codex subagent has no parent thread")
		}
	default:
		return session.SessionOrigin{}, errors.New("Codex session has unknown thread source")
	}
	var source struct {
		Subagent *codexSubagentSource `json:"subagent"`
	}
	raw := bytes.TrimSpace(p.Source)
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &source) != nil || source.Subagent == nil {
		return session.SessionOrigin{}, errors.New("Codex subagent has invalid source provenance")
	}
	spawn := source.Subagent.ThreadSpawn
	guardian := source.Subagent.Other == "guardian"
	if (spawn != nil) == guardian || source.Subagent.Other != "" && !guardian {
		return session.SessionOrigin{}, errors.New("Codex subagent has ambiguous source provenance")
	}
	origin := session.SessionOrigin{ParentSessionID: p.ParentThreadID}
	if spawn != nil {
		if spawn.ParentThreadID != p.ParentThreadID {
			return session.SessionOrigin{}, errors.New("Codex subagent parent evidence conflicts")
		}
		origin.Kind, origin.AgentPath, origin.AgentName, origin.AgentRole = "thread_spawn", spawn.AgentPath, spawn.AgentNickname, spawn.AgentRole
	} else {
		origin.Kind = "guardian"
	}
	return origin, nil
}

func mapCodexResponse(p codexPayload, line int, when time.Time) (session.Event, bool) {
	id := eventID(p.ID, line)
	switch p.Type {
	case "message":
		kind := session.EventAssistant
		if p.Role == "user" {
			if len(p.Content) > 0 && strings.HasPrefix(strings.TrimSpace(p.Content[0].Text), "<environment_context>") {
				return session.Event{}, false
			}
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
	case "custom_tool_call":
		return session.Event{ID: id, Kind: session.EventToolCall, Time: when, ToolName: p.Name, Input: jsonValue(p.Input)}, true
	case "custom_tool_call_output":
		return session.Event{ID: id, ParentID: p.CallID, Kind: session.EventToolResult, Time: when, Output: codexOutputText(p.Output)}, true
	default:
		return session.Event{}, false
	}
}

func codexOutputText(raw json.RawMessage) json.RawMessage {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return jsonValue(raw)
	}
	var content []codexContent
	if json.Unmarshal(raw, &content) != nil {
		return jsonValue(raw)
	}
	var text string
	for _, block := range content {
		if block.Type == "input_text" {
			text += block.Text
		}
	}
	encoded, _ := json.Marshal(text)
	return encoded
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
