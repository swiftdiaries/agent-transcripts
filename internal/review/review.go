package review

import (
	"fmt"
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

// Author is the stable, filterable producer identity of a workspace event.
// Agent IDs stay provider evidence on the underlying event; this projection
// supplies the presentation/filter key without changing that source data.
type Author struct {
	Key     string
	Label   string
	AgentID string
}

type AgentStream struct {
	Key              string
	Label            string
	AgentID          string
	ParentKey        string
	ParentToolCallID string
	Depth            int
	Node             *TranscriptNode
}

type ActivityItem struct {
	Event      session.Event
	Author     Author
	StreamKey  string
	SessionID  string
	Depth      int
	EventIndex int
}

type Workspace struct {
	Family   FamilyTranscript
	Agents   []AgentStream
	Authors  []Author
	Activity []ActivityItem
}

func ProjectWorkspace(family session.SessionFamily) Workspace {
	projected := ProjectFamily(family)
	sessions := map[string]session.Session{family.Main.ID: family.Main}
	for _, child := range family.Children {
		sessions[child.Session.ID] = child.Session
	}
	workspace := Workspace{Family: projected}
	workspace.Authors = append(workspace.Authors,
		Author{Key: "user", Label: "User"},
		Author{Key: "main", Label: "Main agent"},
	)
	var visit func(*TranscriptNode, string, int)
	visit = func(node *TranscriptNode, parentKey string, depth int) {
		key := "main"
		label := "Main agent"
		if node != projected.Root {
			key = "agent:" + node.AgentID
			label = "Agent " + node.AgentID
			workspace.Authors = append(workspace.Authors, Author{Key: key, Label: label, AgentID: node.AgentID})
		}
		workspace.Agents = append(workspace.Agents, AgentStream{
			Key: key, Label: label, AgentID: node.AgentID, ParentKey: parentKey,
			ParentToolCallID: node.ParentToolCallID, Depth: depth, Node: node,
		})
		for index, event := range sessions[node.SessionID].Events {
			author := Author{Key: key, Label: label, AgentID: node.AgentID}
			if event.Kind == session.EventUser {
				author = workspace.Authors[0]
			}
			workspace.Activity = append(workspace.Activity, ActivityItem{
				Event: event, Author: author, StreamKey: key, SessionID: node.SessionID,
				Depth: depth, EventIndex: index,
			})
		}
		attachedIDs := make([]string, 0, len(node.Attached))
		for id := range node.Attached {
			attachedIDs = append(attachedIDs, id)
		}
		sort.Strings(attachedIDs)
		for _, id := range attachedIDs {
			for _, child := range node.Attached[id] {
				visit(child, key, depth+1)
			}
		}
		for _, child := range node.Children {
			visit(child, key, depth+1)
		}
	}
	visit(projected.Root, "", 0)
	sort.SliceStable(workspace.Activity, func(i, j int) bool {
		left, right := workspace.Activity[i], workspace.Activity[j]
		if left.Event.Time.IsZero() || right.Event.Time.IsZero() {
			return !left.Event.Time.IsZero() && right.Event.Time.IsZero()
		}
		if !left.Event.Time.Equal(right.Event.Time) {
			return left.Event.Time.Before(right.Event.Time)
		}
		if left.Depth != right.Depth {
			return left.Depth < right.Depth
		}
		if left.StreamKey != right.StreamKey {
			return left.StreamKey < right.StreamKey
		}
		return left.EventIndex < right.EventIndex
	})
	return workspace
}

func (w Workspace) Stream(key string) (AgentStream, bool) {
	for _, stream := range w.Agents {
		if stream.Key == key {
			return stream, true
		}
	}
	return AgentStream{}, false
}

func (w Workspace) FilterActivity(authorKey string) ([]ActivityItem, error) {
	for _, author := range w.Authors {
		if author.Key == authorKey {
			filtered := make([]ActivityItem, 0)
			for _, item := range w.Activity {
				if item.Author.Key == authorKey {
					filtered = append(filtered, item)
				}
			}
			return filtered, nil
		}
	}
	return nil, fmt.Errorf("unknown workspace author %q", authorKey)
}

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
