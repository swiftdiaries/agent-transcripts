package review

import (
	"sort"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type Turn struct {
	Prompt      session.Event
	Events      []session.Event
	Diagnostics []session.Event
}

type Transcript struct {
	Turns       []Turn
	Diagnostics []session.Event
}

type TranscriptNode struct {
	SessionID        string
	AgentID          string
	ParentToolCallID string
	AgentType        string
	Description      string
	StartedAt        time.Time
	Completion       session.Completion
	Transcript       Transcript
	Attached         map[string][]*TranscriptNode
	Children         []*TranscriptNode
}

type FamilyTranscript struct{ Root *TranscriptNode }

func ProjectFamily(family session.SessionFamily) FamilyTranscript {
	root := transcriptNode(family.Main, "", "", "")
	byParentSessionID := map[string]*TranscriptNode{family.Main.ID: root}
	byAgentID := make(map[string]*TranscriptNode, len(family.Children))
	for _, child := range family.Children {
		node := transcriptNode(child.Session, child.AgentID, child.AgentType, child.Description)
		node.ParentToolCallID = child.ParentToolCallID
		byAgentID[child.AgentID] = node
		if family.Provider == "codex" {
			byParentSessionID[child.Session.ID] = node
		}
	}
	for _, child := range family.Children {
		parentSessionID := child.ParentSessionID
		if parentSessionID == "" {
			parentSessionID = family.Main.ID
		}
		parent := byParentSessionID[parentSessionID]
		node := byAgentID[child.AgentID]
		if child.Attached {
			parent.Attached[child.ParentToolCallID] = append(parent.Attached[child.ParentToolCallID], node)
			continue
		}
		parent.Children = append(parent.Children, node)
	}
	sortTranscriptNode(root)
	return FamilyTranscript{Root: root}
}

func transcriptNode(value session.Session, agentID, agentType, description string) *TranscriptNode {
	return &TranscriptNode{
		SessionID:   value.ID,
		AgentID:     agentID,
		AgentType:   agentType,
		Description: description,
		StartedAt:   value.StartedAt,
		Completion:  value.Completion,
		Transcript:  Project(value),
		Attached:    make(map[string][]*TranscriptNode),
	}
}

func sortTranscriptNode(node *TranscriptNode) {
	sort.Slice(node.Children, func(i, j int) bool { return before(node.Children[i], node.Children[j]) })
	for _, children := range node.Attached {
		sort.Slice(children, func(i, j int) bool { return before(children[i], children[j]) })
		for _, child := range children {
			sortTranscriptNode(child)
		}
	}
	for _, child := range node.Children {
		sortTranscriptNode(child)
	}
}

func before(left, right *TranscriptNode) bool {
	if left.StartedAt.Equal(right.StartedAt) {
		return left.AgentID < right.AgentID
	}
	return left.StartedAt.Before(right.StartedAt)
}

func Project(s session.Session) Transcript {
	var out Transcript
	var current *Turn
	for _, event := range s.Events {
		switch event.Kind {
		case session.EventUser:
			out.Turns = append(out.Turns, Turn{Prompt: event})
			current = &out.Turns[len(out.Turns)-1]
		case session.EventRaw:
			if current == nil {
				out.Diagnostics = append(out.Diagnostics, event)
			} else {
				current.Diagnostics = append(current.Diagnostics, event)
			}
		default:
			if current == nil {
				out.Diagnostics = append(out.Diagnostics, event)
			} else {
				current.Events = append(current.Events, event)
			}
		}
	}
	return out
}
