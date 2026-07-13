package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
)

var ErrIncomplete = errors.New("transcript completion is unproven")

type ImportAttrs struct {
	Destination session.Directory
	UploaderKey string
	Title       string
	Description string
	Tags        []string
	Project     string
}

type Service struct {
	store           store.Store
	parsers         parser.Registry
	now             func() time.Time
	allowLocalQuiet bool
}

type Option func(*Service)

func AllowLocalQuietEvidence() Option { return func(s *Service) { s.allowLocalQuiet = true } }
func New(st store.Store, options ...Option) *Service {
	s := &Service{store: st, parsers: parser.DefaultRegistry(), now: time.Now}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Service) Import(ctx context.Context, source io.Reader, facts session.SourceFacts, attrs ImportAttrs) (session.Metadata, error) {
	if source == nil {
		return session.Metadata{}, errors.New("source is required")
	}
	if err := session.ValidateDirectory(attrs.Destination); err != nil {
		return session.Metadata{}, err
	}
	uploader, err := session.NormalizeUploaderKey(attrs.UploaderKey)
	if err != nil {
		return session.Metadata{}, err
	}
	tags, err := session.NormalizeTags(attrs.Tags)
	if err != nil {
		return session.Metadata{}, err
	}
	tmp, err := os.CreateTemp("", "agent-transcripts-import-*.jsonl")
	if err != nil {
		return session.Metadata{}, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(&contextReader{ctx: ctx, r: source}, session.MaxSourceBytes+1))
	if err != nil {
		return session.Metadata{}, err
	}
	if n > session.MaxSourceBytes {
		return session.Metadata{}, &parser.ErrSourceTooLarge{}
	}
	if facts.ObservedSize != 0 && facts.ObservedSize != n {
		return session.Metadata{}, errors.New("source size differs from descriptor facts")
	}
	facts.ObservedSize = n
	if err := tmp.Sync(); err != nil {
		return session.Metadata{}, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return session.Metadata{}, err
	}
	parsed, err := s.parsers.DetectAndParse(ctx, tmp)
	if err != nil {
		return session.Metadata{}, err
	}
	if !parsed.Completion.Terminal && !(s.allowLocalQuiet && facts.QuietPeriodVerified) {
		return session.Metadata{}, ErrIncomplete
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return session.Metadata{}, err
	}
	raw, err := io.ReadAll(tmp)
	if err != nil {
		return session.Metadata{}, err
	}
	sourceSum := hex.EncodeToString(h.Sum(nil))
	contentID := session.ContentID(parsed.Provider, sourceSum)
	id := session.PackageID(contentID, attrs.Destination)
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return session.Metadata{}, err
	}
	md := session.Metadata{ID: id, ContentID: contentID, Provider: parsed.Provider, ProviderSessionID: parsed.ProviderSessionID, Title: attrs.Title, Description: attrs.Description, Tags: tags, Project: attrs.Project, WorkingDirectory: parsed.WorkingDirectory, StartedAt: parsed.StartedAt, EndedAt: parsed.EndedAt, UploaderKey: uploader, UploadedAt: s.now().UTC(), Destination: attrs.Destination, SourceChecksum: sourceSum, ParserVersion: 1, NormalizedSchemaVersion: parsed.SchemaVersion}
	pkg := session.Package{ID: id, ContentID: contentID, Session: parsed, Metadata: md, Source: raw, Normalized: normalized, SourceFacts: facts}
	if err := session.ValidatePackage(pkg); err != nil {
		return session.Metadata{}, fmt.Errorf("validate imported package: %w", err)
	}
	created, err := s.store.PutSession(ctx, pkg)
	if err != nil {
		return session.Metadata{}, err
	}
	if !created {
		existing, err := s.store.GetSession(ctx, id)
		if err != nil {
			return session.Metadata{}, err
		}
		return existing.Metadata, nil
	}
	stored, err := s.store.GetSession(ctx, id)
	if err != nil {
		return session.Metadata{}, err
	}
	return stored.Metadata, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
