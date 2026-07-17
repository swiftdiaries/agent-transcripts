package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

var ErrSourceChanged = errors.New("transcript source changed after discovery")
var ErrNotEligible = errors.New("transcript source is not complete")

type Roots struct {
	Claude []string
	Codex  []string
}

type Candidate struct {
	Path      string
	Provider  string
	SessionID string
	Project   string
	Title     string
	StartedAt time.Time
	Status    string
	Origin    session.SessionOrigin
	Scope     session.ProjectScope

	quietVerified bool
	invalid       bool
	root          string
	relativePath  string
	identity      fileIdentity
}

// Discover merges eligible transcripts from all configured provider roots.
// Bad or partially-written transcripts are ignored; filesystem walking errors
// are returned because silently losing an entire configured root is misleading.
func Discover(ctx context.Context, roots Roots, now time.Time, quiet time.Duration) ([]Candidate, error) {
	var out []Candidate
	seen := make(map[string]struct{})
	for _, group := range []struct {
		provider string
		roots    []string
	}{{"claude", roots.Claude}, {"codex", roots.Codex}} {
		for _, root := range group.roots {
			if err := walk(ctx, root, group.provider, now, quiet, seen, &out); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func walk(ctx context.Context, root, provider string, now time.Time, quiet time.Duration, seen map[string]struct{}, out *[]Candidate) error {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve discovery root %q: %w", root, err)
	}
	root = absoluteRoot
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect discovery root %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !matches(provider, entry.Name()) {
			return nil
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil
		}
		if _, ok := seen[absolute]; ok {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !safeRelativePath(relative) {
			return nil
		}
		candidate, ok, _ := inspectAt(ctx, root, relative, absolute, provider, now, quiet)
		if ok {
			seen[absolute] = struct{}{}
			*out = append(*out, candidate)
		}
		return nil
	})
}

func matches(provider, name string) bool {
	if !strings.HasSuffix(strings.ToLower(name), ".jsonl") {
		return false
	}
	return provider == "claude" || strings.HasPrefix(name, "rollout-")
}

func inspect(ctx context.Context, path, provider string, now time.Time, quiet time.Duration, _ os.FileInfo) (Candidate, bool, error) {
	return inspectAt(ctx, filepath.Dir(path), filepath.Base(path), path, provider, now, quiet)
}

func inspectAt(ctx context.Context, root, relativePath, path, provider string, now time.Time, quiet time.Duration) (Candidate, bool, error) {
	f, identity, err := safeOpen(root, relativePath)
	if err != nil {
		return Candidate{}, false, safeOpenChanged(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Candidate{}, false, err
	}
	if info.Size() > session.MaxSourceBytes {
		return Candidate{}, false, &parser.ErrSourceTooLarge{}
	}
	parsed, err := parser.DefaultRegistry().DetectAndParse(ctx, f)
	if err != nil {
		if provider == "codex" {
			if candidate, ok := malformedCodexCandidate(f, path, info, root, relativePath, identity); ok {
				return candidate, true, nil
			}
		}
		return Candidate{}, false, err
	}
	if parsed.Provider != provider || !matches(parsed.Provider, filepath.Base(path)) || !hasConversation(parsed) {
		return Candidate{}, false, ErrNotEligible
	}
	quietOK := !now.Before(info.ModTime().Add(quiet))
	if !parsed.Completion.Terminal && !quietOK {
		return Candidate{}, false, ErrNotEligible
	}
	status := "quiet"
	if parsed.Completion.Terminal {
		status = "terminal"
	}
	scope, _ := ResolveProjectScope(parsed.WorkingDirectory)
	return Candidate{Path: path, Provider: provider, SessionID: parsed.ProviderSessionID,
		Project: project(parsed), Title: title(parsed), StartedAt: parsed.StartedAt, Status: status,
		Origin: parsed.Origin, Scope: scope,
		quietVerified: !parsed.Completion.Terminal && quietOK, root: root, relativePath: relativePath, identity: identity}, true, nil
}

// malformedCodexCandidate preserves only an explicit Codex subagent edge when
// strict parsing rejects the record. The candidate is an invalid graph seed,
// never a renderable family; retaining its parent evidence prevents discovery
// from rendering the otherwise-valid ancestor as a partial family.
func malformedCodexCandidate(f *os.File, path string, info os.FileInfo, root, relativePath string, identity fileIdentity) (Candidate, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Candidate{}, false
	}
	var entry struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			ID             string `json:"id"`
			ParentThreadID string `json:"parent_thread_id"`
			WorkingDir     string `json:"cwd"`
			ThreadSource   string `json:"thread_source"`
		} `json:"payload"`
	}
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Type != "session_meta" || entry.Payload.ThreadSource != "subagent" || entry.Payload.ID == "" || entry.Payload.ParentThreadID == "" {
		return Candidate{}, false
	}
	scope, _ := ResolveProjectScope(entry.Payload.WorkingDir)
	startedAt, _ := time.Parse(time.RFC3339, entry.Timestamp)
	return Candidate{
		Path: path, Provider: "codex", SessionID: entry.Payload.ID,
		Project: filepath.Base(filepath.Clean(entry.Payload.WorkingDir)), StartedAt: startedAt,
		Origin: session.SessionOrigin{ParentSessionID: entry.Payload.ParentThreadID}, Scope: scope,
		root: root, relativePath: relativePath, identity: identity, invalid: true,
	}, true
}

func hasConversation(s session.Session) bool {
	for _, event := range s.Events {
		if event.Kind == session.EventUser || event.Kind == session.EventAssistant {
			return true
		}
	}
	return false
}

func project(s session.Session) string {
	if s.Project != "" {
		return s.Project
	}
	if s.WorkingDirectory == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(s.WorkingDirectory))
}

func title(s session.Session) string {
	for _, event := range s.Events {
		if event.Kind == session.EventUser && strings.TrimSpace(event.Text) != "" {
			value := strings.ToValidUTF8(strings.TrimSpace(event.Text), "�")
			if len(value) > session.MaxTitleBytes {
				value = value[:session.MaxTitleBytes]
				for !utf8.ValidString(value) {
					value = value[:len(value)-1]
				}
			}
			return value
		}
	}
	return ""
}

// OpenEligible opens exactly once and validates the opened descriptor against
// discovery facts. The caller must parse/import the returned reader directly.
func OpenEligible(candidate Candidate) (io.ReadCloser, session.SourceFacts, error) {
	f, identity, err := candidate.openVerified()
	if err != nil {
		return nil, session.SourceFacts{}, err
	}
	if identity.Size > session.MaxSourceBytes {
		f.Close()
		return nil, session.SourceFacts{}, &parser.ErrSourceTooLarge{}
	}
	// Recheck that the exact bytes behind the descriptor still contain valid
	// completion evidence (or retain the previously verified quiet snapshot).
	parsed, err := parser.DefaultRegistry().DetectAndParse(context.Background(), f)
	if err != nil || parsed.Provider != candidate.Provider || !hasConversation(parsed) || (!parsed.Completion.Terminal && !candidate.quietVerified) {
		f.Close()
		return nil, session.SourceFacts{}, ErrNotEligible
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, session.SourceFacts{}, err
	}
	return f, sourceFacts(identity, candidate.quietVerified), nil
}

func sourceFacts(identity fileIdentity, quiet bool) session.SourceFacts {
	return session.SourceFacts{ObservedModTime: time.Unix(0, identity.ModTimeNS), ObservedSize: identity.Size, QuietPeriodVerified: quiet}
}

func safeRelativePath(relative string) bool {
	if relative == "" || filepath.IsAbs(relative) {
		return false
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// InspectPath applies the same completion gate used by root discovery to an
// explicitly supplied source path.
func InspectPath(ctx context.Context, path string, now time.Time, quiet time.Duration) (Candidate, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Candidate{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return Candidate{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Candidate{}, ErrNotEligible
	}
	for _, provider := range []string{"claude", "codex"} {
		candidate, ok, inspectErr := inspect(ctx, abs, provider, now, quiet, info)
		if ok {
			return candidate, nil
		}
		var tooLarge *parser.ErrSourceTooLarge
		if errors.As(inspectErr, &tooLarge) {
			return Candidate{}, inspectErr
		}
	}
	return Candidate{}, ErrNotEligible
}
