package parser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type ErrSourceTooLarge struct{}

func (*ErrSourceTooLarge) Error() string {
	return fmt.Sprintf("source exceeds %d bytes", session.MaxSourceBytes)
}

type ErrRecordTooLarge struct{}

func (*ErrRecordTooLarge) Error() string {
	return fmt.Sprintf("record exceeds %d bytes", session.MaxRecordBytes)
}

type Parser interface {
	Provider() string
	Detect(first json.RawMessage) bool
	Parse(ctx context.Context, lines []json.RawMessage) (session.Session, error)
}

type Registry struct{ parsers []Parser }

func DefaultRegistry() Registry { return Registry{parsers: []Parser{claudeParser{}, codexParser{}}} }

func (r Registry) DetectAndParse(ctx context.Context, source io.Reader) (session.Session, error) {
	if err := ctx.Err(); err != nil {
		return session.Session{}, err
	}
	limited := &countingReader{r: io.LimitReader(source, session.MaxSourceBytes+1)}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64<<10), session.MaxRecordBytes+1)
	var lines []json.RawMessage
	var first json.RawMessage
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		if limited.n > session.MaxSourceBytes {
			return session.Session{}, &ErrSourceTooLarge{}
		}
		if err := ctx.Err(); err != nil {
			return session.Session{}, err
		}
		b := scanner.Bytes()
		if len(b) > session.MaxRecordBytes {
			return session.Session{}, &ErrRecordTooLarge{}
		}
		if strings.TrimSpace(string(b)) == "" {
			lines = append(lines, nil)
			continue
		}
		if !json.Valid(b) {
			return session.Session{}, fmt.Errorf("malformed JSON at line %d", lineNumber)
		}
		copyOfLine := append(json.RawMessage(nil), b...)
		lines = append(lines, copyOfLine)
		if first == nil {
			first = copyOfLine
		}
	}
	if limited.n > session.MaxSourceBytes {
		return session.Session{}, &ErrSourceTooLarge{}
	}
	if err := scanner.Err(); err != nil {
		return session.Session{}, &ErrRecordTooLarge{}
	}
	if first == nil {
		return session.Session{}, errors.New("source contains no JSON records")
	}
	for _, line := range lines {
		if line == nil {
			continue
		}
		for _, parser := range r.parsers {
			if !parser.Detect(line) {
				continue
			}
			got, err := parser.Parse(ctx, lines)
			if err != nil {
				return session.Session{}, err
			}
			if err := session.Validate(got); err != nil {
				return session.Session{}, fmt.Errorf("validate %s session: %w", parser.Provider(), err)
			}
			return got, nil
		}
	}
	return session.Session{}, errors.New("unrecognized transcript provider")
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

type envelope struct {
	Type          string          `json:"type"`
	Timestamp     string          `json:"timestamp"`
	UUID          string          `json:"uuid"`
	ParentUUID    string          `json:"parentUuid"`
	SessionID     string          `json:"sessionId"`
	CWD           string          `json:"cwd"`
	Subtype       string          `json:"subtype"`
	Message       json.RawMessage `json:"message"`
	Payload       json.RawMessage `json:"payload"`
	AgentID       string          `json:"agentId"`
	ToolUseResult struct {
		AgentID string `json:"agentId"`
	} `json:"toolUseResult"`
}

type ClaudeChild struct {
	AgentID string
	Session session.Session
}

// AttachClaudeChildren proves a parent-child relationship solely through the
// provider agent ID on a tool result and its referenced Agent or Task call.
func AttachClaudeChildren(main session.Session, children []ClaudeChild) ([]session.ChildSession, error) {
	result := make([]session.ChildSession, 0, len(children))
	seen := make(map[string]struct{}, len(children))
	for _, child := range children {
		if child.AgentID == "" {
			return nil, errors.New("Claude child has no agent ID")
		}
		if _, exists := seen[child.AgentID]; exists {
			return nil, fmt.Errorf("duplicate Claude child agent ID %q", child.AgentID)
		}
		seen[child.AgentID] = struct{}{}
		parentID := ""
		for _, event := range main.Events {
			if event.Kind == session.EventToolResult && event.AgentID == child.AgentID {
				if parentID != "" {
					return nil, fmt.Errorf("ambiguous Claude parent result for agent %q", child.AgentID)
				}
				parentID = event.ParentID
			}
		}
		attached := false
		if parentID != "" {
			for _, event := range main.Events {
				if event.ID == parentID && event.Kind == session.EventToolCall && (event.ToolName == "Agent" || event.ToolName == "Task") {
					attached = true
				}
			}
		}
		entry := session.ChildSession{AgentID: child.AgentID, Attached: attached, Session: child.Session}
		if attached {
			entry.ParentToolCallID = parentID
		}
		result = append(result, entry)
	}
	return result, nil
}
