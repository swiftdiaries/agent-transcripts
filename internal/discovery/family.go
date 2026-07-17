package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type SourceCandidate struct{ Candidate }
type ChildSourceCandidate struct {
	Candidate       Candidate
	AgentID         string
	ParentSessionID string
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
	var result []SessionFamilyCandidate
	var codex []Candidate
	for _, candidate := range candidates {
		if candidate.Provider == "codex" {
			codex = append(codex, candidate)
			continue
		}
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
	if len(codex) != 0 {
		families, err := formCodexFamilies(codex, scope)
		if err != nil {
			return nil, err
		}
		result = append(result, families...)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result, nil
}

func formCodexFamilies(candidates []Candidate, scope session.ProjectScope) ([]SessionFamilyCandidate, error) {
	counts := make(map[string]int, len(candidates))
	byID := make(map[string]Candidate, len(candidates))
	for _, candidate := range candidates {
		counts[candidate.SessionID]++
		byID[candidate.SessionID] = candidate
	}
	invalid := make(map[string]bool)
	for id, count := range counts {
		if count != 1 {
			invalid[id] = true
		}
	}
	for _, candidate := range candidates {
		if candidate.invalid {
			invalid[candidate.SessionID] = true
		}
	}
	for id, candidate := range byID {
		if parent := candidate.Origin.ParentSessionID; parent != "" && (counts[parent] != 1 || invalid[parent]) {
			invalid[id] = true
		}
	}
	markCodexCycles(byID, invalid)
	markCrossProjectCodexComponents(candidates, byID, invalid, scope)
	propagateInvalidCodexParents(byID, invalid)
	return buildCodexRootFamilies(byID, invalid, scope), nil
}

// markCrossProjectCodexComponents rejects a whole connected parent graph when
// one parent-child edge spans canonical project scopes. Keeping only the root
// would otherwise expose a partial family after callers partition candidates.
func markCrossProjectCodexComponents(candidates []Candidate, byID map[string]Candidate, invalid map[string]bool, fallback session.ProjectScope) {
	neighbors := make(map[string]map[string]struct{}, len(byID))
	queue := make([]string, 0, len(invalid))
	for id := range invalid {
		queue = append(queue, id)
	}
	for _, candidate := range candidates {
		id := candidate.SessionID
		parent := candidate.Origin.ParentSessionID
		parentCandidate, ok := byID[parent]
		if parent == "" || !ok {
			continue
		}
		if neighbors[id] == nil {
			neighbors[id] = make(map[string]struct{})
		}
		if neighbors[parent] == nil {
			neighbors[parent] = make(map[string]struct{})
		}
		neighbors[id][parent] = struct{}{}
		neighbors[parent][id] = struct{}{}
		if codexCandidateScope(candidate, fallback).Ref.Key != codexCandidateScope(parentCandidate, fallback).Ref.Key {
			queue = append(queue, id, parent)
		}
	}
	seen := make(map[string]bool, len(queue))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		invalid[id] = true
		for neighbor := range neighbors[id] {
			if !seen[neighbor] {
				queue = append(queue, neighbor)
			}
		}
	}
}

func codexCandidateScope(candidate Candidate, fallback session.ProjectScope) session.ProjectScope {
	if candidate.Scope.Ref.Key != "" {
		return candidate.Scope
	}
	return fallback
}

func markCodexCycles(byID map[string]Candidate, invalid map[string]bool) {
	const (
		white = iota
		gray
		black
	)
	state := make(map[string]int, len(byID))
	var stack []string
	stackIndex := make(map[string]int, len(byID))
	var visit func(string)
	visit = func(id string) {
		if invalid[id] || state[id] == black {
			return
		}
		state[id] = gray
		stackIndex[id] = len(stack)
		stack = append(stack, id)
		parent := byID[id].Origin.ParentSessionID
		if parent != "" && !invalid[parent] {
			switch state[parent] {
			case white:
				if _, ok := byID[parent]; ok {
					visit(parent)
				}
			case gray:
				for _, cycleID := range stack[stackIndex[parent]:] {
					invalid[cycleID] = true
				}
			}
		}
		stack = stack[:len(stack)-1]
		delete(stackIndex, id)
		state[id] = black
	}
	for id := range byID {
		visit(id)
	}
}

func propagateInvalidCodexParents(byID map[string]Candidate, invalid map[string]bool) {
	changed := true
	for changed {
		changed = false
		for id, candidate := range byID {
			parent := candidate.Origin.ParentSessionID
			if parent != "" && invalid[parent] && !invalid[id] {
				invalid[id] = true
				changed = true
			}
		}
	}
}

func buildCodexRootFamilies(byID map[string]Candidate, invalid map[string]bool, scope session.ProjectScope) []SessionFamilyCandidate {
	childrenByParent := make(map[string][]Candidate, len(byID))
	var roots []Candidate
	for id, candidate := range byID {
		if invalid[id] {
			continue
		}
		if parent := candidate.Origin.ParentSessionID; parent == "" {
			roots = append(roots, candidate)
		} else {
			childrenByParent[parent] = append(childrenByParent[parent], candidate)
		}
	}
	for parent := range childrenByParent {
		sort.Slice(childrenByParent[parent], func(i, j int) bool {
			return childrenByParent[parent][i].SessionID < childrenByParent[parent][j].SessionID
		})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].SessionID < roots[j].SessionID })
	result := make([]SessionFamilyCandidate, 0, len(roots))
	for _, root := range roots {
		familyScope := codexCandidateScope(root, scope)
		family := SessionFamilyCandidate{Key: familyKey(root.Provider, root.Path), Provider: root.Provider, ProviderSessionID: root.SessionID, Project: familyScope.Ref, Title: root.Title, StartedAt: root.StartedAt, Status: root.Status, Main: SourceCandidate{Candidate: root}}
		var appendDescendants func(string)
		appendDescendants = func(parent string) {
			for _, child := range childrenByParent[parent] {
				family.Children = append(family.Children, ChildSourceCandidate{Candidate: child, ParentSessionID: child.Origin.ParentSessionID})
				appendDescendants(child.SessionID)
			}
		}
		appendDescendants(root.SessionID)
		sort.Slice(family.Children, func(i, j int) bool {
			return family.Children[i].Candidate.SessionID < family.Children[j].Candidate.SessionID
		})
		result = append(result, family)
	}
	return result
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
	var codex []Candidate
	for _, candidate := range candidates {
		memberScope := candidate.Scope
		if memberScope.Ref.Key == "" {
			continue
		}
		if candidate.Provider == "codex" {
			codex = append(codex, candidate)
			continue
		}
		group := byProject[memberScope.Ref.Key]
		group.scope = memberScope
		group.candidates = append(group.candidates, candidate)
		byProject[memberScope.Ref.Key] = group
	}
	var result []SessionFamilyCandidate
	for _, group := range byProject {
		families, err := FormFamilies(group.candidates, group.scope)
		if err != nil {
			return nil, err
		}
		result = append(result, families...)
	}
	if len(codex) != 0 {
		families, err := formCodexFamilies(codex, session.ProjectScope{})
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
