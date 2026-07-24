package parser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   claudeUsage     `json:"usage"`
}

type claudeUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}
type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	Thinking  string          `json:"thinking"`
}

type claudeToolUseResult struct {
	AgentID string `json:"agentId"`
	Status  string `json:"status"`
}

func (claudeParser) Parse(ctx context.Context, lines []json.RawMessage) (session.Session, error) {
	got := session.Session{SchemaVersion: 1, Provider: "claude"}
	usageIndex := map[string]int{}
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
		if e.SessionID != "" && got.ProviderSessionID != "" && e.SessionID != got.ProviderSessionID {
			return session.Session{}, fmt.Errorf("Claude line %d session ID does not match source session", lineNumber)
		}
		if got.WorkingDirectory == "" {
			got.WorkingDirectory = e.CWD
		}
		if e.Type == "system" && e.Subtype == "turn_duration" {
			got.Completion.Terminal, got.Completion.TerminalReason = true, e.Subtype
			continue
		}
		var message claudeMessage
		if e.Type == "user" || e.Type == "assistant" {
			if err := json.Unmarshal(e.Message, &message); err != nil {
				return session.Session{}, fmt.Errorf("decode Claude message at line %d: %w", lineNumber, err)
			}
		}
		if e.Type == "user" && isClaudeInjectedInstructions(message) {
			got.Events = append(got.Events, rawEvent(eventID(e.UUID, lineNumber), e.ParentUUID, "claude_injected_instructions", when, line))
			continue
		}
		events, mapped, err := mapClaudeMessage(e, message, lineNumber, when)
		if err != nil {
			return session.Session{}, err
		}
		if mapped {
			got.Events = append(got.Events, events...)
			if e.Type == "assistant" {
				sampleID := message.ID
				if sampleID == "" {
					sampleID = eventID(e.UUID, lineNumber)
				}
				sample := session.UsageSample{
					ID: sampleID, Time: when, Model: message.Model,
					Tokens: session.TokenUsage{Input: message.Usage.Input, Output: message.Usage.Output, CacheRead: message.Usage.CacheRead, CacheWrite: message.Usage.CacheWrite},
				}
				if sample.Tokens.Total() > 0 {
					if index, ok := usageIndex[sampleID]; ok {
						if got.Usage[index].Model != "" && sample.Model != "" && got.Usage[index].Model != sample.Model {
							return session.Session{}, fmt.Errorf("Claude line %d usage model conflicts message %q", lineNumber, sampleID)
						}
						got.Usage[index] = sample
					} else {
						usageIndex[sampleID] = len(got.Usage)
						got.Usage = append(got.Usage, sample)
					}
				}
			}
			continue
		}
		got.Events = append(got.Events, rawEvent(eventID(e.UUID, lineNumber), e.ParentUUID, e.Type, when, line))
	}
	if got.ID == "" {
		got.ID = "session-line-1"
	}
	return got, nil
}

func isClaudeInjectedInstructions(message claudeMessage) bool {
	var text string
	if json.Unmarshal(message.Content, &text) != nil {
		return false
	}
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "# AGENTS.md instructions") &&
		strings.Contains(text, "<!-- headroom:rtk-instructions -->") &&
		strings.Contains(text, "# RTK (Rust Token Killer) - Token-Optimized Commands")
}

func mapClaudeMessage(e envelope, message claudeMessage, line int, when time.Time) ([]session.Event, bool, error) {
	if e.Type != "user" && e.Type != "assistant" {
		return nil, false, nil
	}
	var text string
	if err := json.Unmarshal(message.Content, &text); err == nil {
		kind := session.EventUser
		if e.Type == "assistant" {
			kind = session.EventAssistant
		}
		return []session.Event{{ID: eventID(e.UUID, line), ParentID: e.ParentUUID, Kind: kind, Time: when, Text: text}}, true, nil
	}
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(message.Content, &rawBlocks); err != nil {
		return nil, false, fmt.Errorf("decode Claude content at line %d: %w", line, err)
	}
	var events []session.Event
	var toolUseResult claudeToolUseResult
	_ = json.Unmarshal(e.ToolUseResult, &toolUseResult)
	for blockIndex, rawBlock := range rawBlocks {
		var block claudeBlock
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			return nil, false, fmt.Errorf("decode Claude content block %d at line %d: %w", blockIndex+1, line, err)
		}
		blockID := block.ID
		if blockID == "" {
			blockID = blockFallbackID(line, blockIndex)
		}
		switch block.Type {
		case "thinking":
			events = append(events, rawEvent(blockID, e.ParentUUID, block.Type, when, rawBlock))
		case "text":
			kind := session.EventUser
			if e.Type == "assistant" {
				kind = session.EventAssistant
			}
			events = append(events, session.Event{ID: blockID, ParentID: e.ParentUUID, Kind: kind, Time: when, Text: block.Text})
		case "tool_use":
			events = append(events, session.Event{ID: blockID, ParentID: e.UUID, Kind: session.EventToolCall, Time: when, ToolName: block.Name, Input: block.Input})
		case "tool_result":
			events = append(events, session.Event{ID: blockID, ParentID: block.ToolUseID, AgentID: toolUseResult.AgentID, Kind: session.EventToolResult, Time: when, Output: jsonValue(block.Content), ResultStatus: toolUseResult.Status})
		default:
			return nil, false, nil
		}
	}
	return events, true, nil
}
