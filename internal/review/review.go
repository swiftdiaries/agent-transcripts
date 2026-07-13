package review

import "github.com/swiftdiaries/agent-transcripts/internal/session"

type Turn struct {
	Prompt      session.Event
	Events      []session.Event
	Diagnostics []session.Event
}

type Transcript struct {
	Turns       []Turn
	Diagnostics []session.Event
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
