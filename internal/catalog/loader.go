package catalog

import (
	"context"
	"errors"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"sync"
	"time"
)

type ParseFamilyFunc func(context.Context, discovery.SessionFamilyCandidate) (session.SessionFamily, error)
type LoadResult struct {
	Candidate discovery.SessionFamilyCandidate
	Family    session.SessionFamily
	Err       error
}
type Loader struct {
	Cache *Cache
	Parse ParseFamilyFunc
}

func NewLoader(cache *Cache) *Loader { return &Loader{Cache: cache, Parse: ParseFamily} }
func (l *Loader) Load(ctx context.Context, c discovery.SessionFamilyCandidate) (session.SessionFamily, error) {
	key := c.RevisionKey(parser.NormalizationVersion)
	if l.Cache != nil {
		if f, ok := l.Cache.Get(key); ok {
			return f, nil
		}
	}
	f, e := l.Parse(ctx, c)
	if e != nil {
		return f, e
	}
	if e = session.ValidateFamily(f); e != nil {
		return f, e
	}
	if l.Cache != nil {
		e = l.Cache.Put(key, f)
	}
	return f, e
}
func (l *Loader) LoadMany(ctx context.Context, cs []discovery.SessionFamilyCandidate) ([]LoadResult, error) {
	out := make([]LoadResult, len(cs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				f, e := l.Load(ctx, cs[i])
				out[i] = LoadResult{cs[i], f, e}
			}
		}()
	}
	for i := range cs {
		if ctx.Err() != nil {
			close(jobs)
			wg.Wait()
			return out, ctx.Err()
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, nil
}
func ParseFamily(ctx context.Context, c discovery.SessionFamilyCandidate) (session.SessionFamily, error) {
	snap, e := discovery.SnapshotFamily(ctx, c)
	if e != nil {
		return session.SessionFamily{}, e
	}
	defer snap.Close()
	parse := func(s discovery.SnapshotSource) (session.Session, error) {
		r, e := s.Open()
		if e != nil {
			return session.Session{}, e
		}
		defer r.Close()
		return parser.DefaultRegistry().DetectAndParse(ctx, r)
	}
	main, e := parse(snap.Sources[0])
	if e != nil {
		return session.SessionFamily{}, e
	}
	f := session.SessionFamily{SchemaVersion: 2, ID: main.ProviderSessionID, Provider: main.Provider, ProviderSessionID: main.ProviderSessionID, Project: c.Project, Main: main}
	var cc []parser.ClaudeChild
	var codex []session.Session
	for i, child := range c.Children {
		p, e := parse(snap.Sources[i+1])
		if e != nil {
			return f, e
		}
		if main.Provider == "claude" {
			cc = append(cc, parser.ClaudeChild{AgentID: child.AgentID, Session: p})
		} else if main.Provider == "codex" {
			codex = append(codex, p)
		} else {
			return f, errors.New("provider does not support family children")
		}
	}
	if main.Provider == "claude" {
		f.Children, e = parser.AttachClaudeChildren(main, cc)
	} else if main.Provider == "codex" {
		f.Children, e = parser.AttachCodexChildren(main, codex)
	}
	if e != nil {
		return f, e
	}
	members := append([]session.Session{f.Main}, childSessions(f.Children)...)
	for _, m := range members {
		if !m.StartedAt.IsZero() && (f.StartedAt.IsZero() || m.StartedAt.Before(f.StartedAt)) {
			f.StartedAt = m.StartedAt
		}
		end := m.EndedAt
		if end.IsZero() {
			end = m.Completion.LastEventAt
		}
		if !end.IsZero() && (f.EndedAt.IsZero() || end.After(f.EndedAt)) {
			f.EndedAt = end
		}
		if !m.Completion.LastEventAt.IsZero() && (f.Completion.LastEventAt.IsZero() || m.Completion.LastEventAt.After(f.Completion.LastEventAt)) {
			f.Completion.LastEventAt = m.Completion.LastEventAt
		}
	}
	all := true
	for _, m := range members {
		all = all && m.Completion.Terminal
	}
	if all {
		f.Completion.Status = "provider_terminal"
		f.Completion.Reason = "all_members_terminal"
	} else {
		f.Completion.Status = "incomplete"
	}
	return f, nil
}
func childSessions(c []session.ChildSession) []session.Session {
	r := make([]session.Session, len(c))
	for i := range c {
		r[i] = c[i].Session
	}
	return r
}

var _ = time.Time{}
