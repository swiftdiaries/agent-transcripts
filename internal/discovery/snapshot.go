package discovery

import (
	"fmt"
	"io"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type SnapshotSource struct {
	Role    string
	AgentID string
	Bytes   []byte
	Facts   session.SourceFacts
}

type FamilySnapshot struct {
	Candidate SessionFamilyCandidate
	Sources   []SnapshotSource
}

// SnapshotFamily reads every already-eligible member before returning any
// source bytes. The aggregate cap prevents child enumeration multiplying the
// single-source memory budget.
func SnapshotFamily(candidate SessionFamilyCandidate) (FamilySnapshot, error) {
	members := append([]ChildSourceCandidate(nil), candidate.Children...)
	if len(members)+1 > session.MaxFamilySources {
		return FamilySnapshot{}, fmt.Errorf("family exceeds %d sources", session.MaxFamilySources)
	}
	out := FamilySnapshot{Candidate: candidate, Sources: make([]SnapshotSource, 0, len(members)+1)}
	remaining := int64(session.MaxSourceBytes)
	copySource := func(role, agentID string, source Candidate) error {
		r, facts, err := OpenEligible(source)
		if err != nil {
			return err
		}
		defer r.Close()
		data, err := io.ReadAll(io.LimitReader(r, remaining+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > remaining {
			return fmt.Errorf("family exceeds aggregate source limit")
		}
		remaining -= int64(len(data))
		out.Sources = append(out.Sources, SnapshotSource{Role: role, AgentID: agentID, Bytes: data, Facts: facts})
		return nil
	}
	if err := copySource("main", "", candidate.Main.Candidate); err != nil {
		return FamilySnapshot{}, err
	}
	for _, child := range members {
		if err := copySource("child", child.AgentID, child.Candidate); err != nil {
			return FamilySnapshot{}, err
		}
	}
	return out, nil
}
