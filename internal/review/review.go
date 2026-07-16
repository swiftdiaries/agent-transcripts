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

type ChildTranscript struct {
	AgentID          string
	ParentToolCallID string
	AgentType        string
	Description      string
	StartedAt        time.Time
	Completion       session.Completion
	Transcript       Transcript
}

type FamilyTranscript struct {
	Main       Transcript
	Attached   map[string][]ChildTranscript
	Unattached []ChildTranscript
}

func ProjectFamily(family session.SessionFamily) FamilyTranscript {
	out := FamilyTranscript{Main: Project(family.Main), Attached: make(map[string][]ChildTranscript)}
	for _, child := range family.Children {
		projected := ChildTranscript{AgentID: child.AgentID, ParentToolCallID: child.ParentToolCallID, AgentType: child.AgentType, Description: child.Description, StartedAt: child.Session.StartedAt, Completion: child.Session.Completion, Transcript: Project(child.Session)}
		if child.Attached {
			out.Attached[child.ParentToolCallID] = append(out.Attached[child.ParentToolCallID], projected)
		} else {
			out.Unattached = append(out.Unattached, projected)
		}
	}
	sort.Slice(out.Unattached, func(i, j int) bool {
		if out.Unattached[i].StartedAt.Equal(out.Unattached[j].StartedAt) {
			return out.Unattached[i].AgentID < out.Unattached[j].AgentID
		}
		return out.Unattached[i].StartedAt.Before(out.Unattached[j].StartedAt)
	})
	return out
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
