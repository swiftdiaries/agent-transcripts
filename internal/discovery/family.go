package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type SourceCandidate struct{ Candidate }
type ChildSourceCandidate struct {
	Candidate Candidate
	AgentID   string
}
type SessionFamilyCandidate struct {
	Key               string
	Provider          string
	ProviderSessionID string
	Project           session.ProjectRef
	Title             string
	StartedAt         time.Time
	Status            string
	Main              SourceCandidate
	Children          []ChildSourceCandidate
}

// FormFamilies turns provider candidates into a single main record with its
// native Claude subagent files. A child never becomes a top-level family.
func FormFamilies(candidates []Candidate, scope session.ProjectScope) ([]SessionFamilyCandidate, error) {
	byPath := make(map[string]Candidate, len(candidates))
	for _, candidate := range candidates {
		byPath[filepath.Clean(candidate.Path)] = candidate
	}
	var result []SessionFamilyCandidate
	for _, candidate := range candidates {
		if candidate.Provider == "claude" && claudeChildIdentity(candidate.Path) != "" {
			continue
		}
		key := familyKey(candidate.Provider, candidate.Path)
		family := SessionFamilyCandidate{Key: key, Provider: candidate.Provider, ProviderSessionID: candidate.SessionID, Project: scope.Ref, Title: candidate.Title, StartedAt: candidate.StartedAt, Status: candidate.Status, Main: SourceCandidate{Candidate: candidate}}
		if candidate.Provider == "claude" {
			for _, possible := range candidates {
				agentID := claudeChildIdentity(possible.Path)
				if agentID == "" || possible.Provider != "claude" || possible.SessionID != candidate.SessionID {
					continue
				}
				if filepath.Clean(filepath.Dir(filepath.Dir(filepath.Dir(possible.Path)))) != filepath.Clean(filepath.Dir(candidate.Path)) {
					continue
				}
				if filepath.Base(filepath.Dir(filepath.Dir(possible.Path))) != strings.TrimSuffix(filepath.Base(candidate.Path), ".jsonl") {
					continue
				}
				family.Children = append(family.Children, ChildSourceCandidate{Candidate: possible, AgentID: agentID})
			}
			sort.Slice(family.Children, func(i, j int) bool { return family.Children[i].AgentID < family.Children[j].AgentID })
		}
		result = append(result, family)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

// DiscoverFamilies applies current-project authorization before returning a
// provider family. It intentionally re-parses candidates to obtain provider
// CWD provenance rather than trusting encoded directory names.
func DiscoverFamilies(ctx context.Context, roots Roots, scope session.ProjectScope, now time.Time, quiet time.Duration) ([]SessionFamilyCandidate, error) {
	all, err := DiscoverAllFamilies(ctx, roots, now, quiet)
	if err != nil {
		return nil, err
	}
	result := make([]SessionFamilyCandidate, 0, len(all))
	for _, family := range all {
		if family.Project.Key == scope.Ref.Key {
			result = append(result, family)
		}
	}
	return result, nil
}

// DiscoverAllFamilies returns eligible session families grouped by their
// canonical project identity. It is reserved for explicit cross-project flows.
func DiscoverAllFamilies(ctx context.Context, roots Roots, now time.Time, quiet time.Duration) ([]SessionFamilyCandidate, error) {
	candidates, err := Discover(ctx, roots, now, quiet)
	if err != nil {
		return nil, err
	}
	byProject := make(map[string]struct {
		scope      session.ProjectScope
		candidates []Candidate
	})
	for _, candidate := range candidates {
		f, err := os.Open(candidate.Path)
		if err != nil {
			continue
		}
		parsed, parseErr := parser.DefaultRegistry().DetectAndParse(ctx, f)
		_ = f.Close()
		if parseErr != nil || parsed.WorkingDirectory == "" {
			continue
		}
		memberScope, scopeErr := ResolveProjectScope(parsed.WorkingDirectory)
		if scopeErr == nil {
			group := byProject[memberScope.Ref.Key]
			group.scope = memberScope
			group.candidates = append(group.candidates, candidate)
			byProject[memberScope.Ref.Key] = group
		}
	}
	var result []SessionFamilyCandidate
	for _, group := range byProject {
		families, err := FormFamilies(group.candidates, group.scope)
		if err != nil {
			return nil, err
		}
		result = append(result, families...)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].StartedAt.Equal(result[j].StartedAt) {
			return result[i].StartedAt.After(result[j].StartedAt)
		}
		return result[i].Key < result[j].Key
	})
	return result, nil
}

func claudeChildIdentity(path string) string {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "agent-") || !strings.HasSuffix(base, ".jsonl") {
		return ""
	}
	if filepath.Base(filepath.Dir(path)) != "subagents" {
		return ""
	}
	value := strings.TrimSuffix(strings.TrimPrefix(base, "agent-"), ".jsonl")
	if value == "" {
		return ""
	}
	return value
}

func familyKey(provider, mainPath string) string {
	canonical, err := filepath.EvalSymlinks(mainPath)
	if err != nil {
		canonical = filepath.Clean(mainPath)
	}
	sum := sha256.Sum256([]byte(provider + "\x00" + canonical))
	return "f_" + hex.EncodeToString(sum[:])
}
