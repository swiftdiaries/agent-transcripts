package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type SnapshotSource struct {
	Role    string
	AgentID string
	Facts   session.SourceFacts
	path    string
}

func (s SnapshotSource) Open() (io.ReadCloser, error) { return os.Open(s.path) }

type FamilySnapshot struct {
	Candidate SessionFamilyCandidate
	Sources   []SnapshotSource
	dir       string
	closeOnce sync.Once
	closeErr  error
}

func (s *FamilySnapshot) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = os.RemoveAll(s.dir) })
	return s.closeErr
}

type SnapshotInput struct {
	Role, AgentID string
	Reader        io.Reader
	Facts         session.SourceFacts
}

var snapshotTestHook func()

// SnapshotFamily captures verified local descriptors in private files. It
// copies each descriptor once, then hashes the same open descriptor again to
// reject writes that occurred during the first pass.
func SnapshotFamily(ctx context.Context, candidate SessionFamilyCandidate) (*FamilySnapshot, error) {
	inputs := append([]ChildSourceCandidate(nil), candidate.Children...)
	if len(inputs)+1 > session.MaxFamilySources {
		return nil, fmt.Errorf("family exceeds %d sources", session.MaxFamilySources)
	}
	snapshot, err := newSnapshot(candidate, len(inputs)+1)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*FamilySnapshot, error) { _ = snapshot.Close(); return nil, err }
	remaining := int64(session.MaxSourceBytes)
	copyCandidate := func(role, agentID string, source Candidate) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, before, err := source.openVerified()
		if err != nil {
			return err
		}
		defer f.Close()
		dst, err := snapshotFile(snapshot.dir, len(snapshot.Sources))
		if err != nil {
			return err
		}
		defer dst.Close()
		copyHash, _, err := copySnapshotSource(ctx, dst, f, &remaining)
		if err != nil {
			return err
		}
		if snapshotTestHook != nil {
			snapshotTestHook()
		}
		afterCopy, err := identityForFile(f)
		if err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		verifyHash, err := hashDescriptor(ctx, f, session.MaxSourceBytes)
		if err != nil {
			return err
		}
		afterHash, err := identityForFile(f)
		if err != nil {
			return err
		}
		if copyHash != verifyHash || !sameIdentity(before, afterCopy) || !sameIdentity(before, afterHash) {
			return ErrSourceChanged
		}
		snapshot.Sources = append(snapshot.Sources, SnapshotSource{Role: role, AgentID: agentID, Facts: sourceFacts(before, source.quietVerified), path: dst.Name()})
		return nil
	}
	if err := copyCandidate("main", "", candidate.Main.Candidate); err != nil {
		return fail(err)
	}
	for _, child := range inputs {
		if err := copyCandidate("child", child.AgentID, child.Candidate); err != nil {
			return fail(err)
		}
	}
	return snapshot, nil
}

// SnapshotReaders owns a private copy of transport-authoritative readers.
func SnapshotReaders(ctx context.Context, candidate SessionFamilyCandidate, inputs []SnapshotInput) (*FamilySnapshot, error) {
	if err := validateSnapshotInputs(inputs); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := newSnapshot(candidate, len(inputs))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*FamilySnapshot, error) { _ = snapshot.Close(); return nil, err }
	remaining := int64(session.MaxSourceBytes)
	for i, input := range inputs {
		if input.Reader == nil {
			return fail(errors.New("snapshot source reader is required"))
		}
		dst, err := snapshotFile(snapshot.dir, i)
		if err != nil {
			return fail(err)
		}
		_, count, copyErr := copySnapshotSource(ctx, dst, input.Reader, &remaining)
		closeErr := dst.Close()
		if copyErr != nil {
			return fail(copyErr)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		facts := input.Facts
		facts.ObservedSize = count
		snapshot.Sources = append(snapshot.Sources, SnapshotSource{Role: input.Role, AgentID: input.AgentID, Facts: facts, path: dst.Name()})
	}
	return snapshot, nil
}

func newSnapshot(candidate SessionFamilyCandidate, capacity int) (*FamilySnapshot, error) {
	dir, err := os.MkdirTemp("", "agent-transcripts-snapshot-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &FamilySnapshot{Candidate: candidate, Sources: make([]SnapshotSource, 0, capacity), dir: dir}, nil
}

func snapshotFile(dir string, index int) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, fmt.Sprintf("source-%03d.jsonl", index)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func copySnapshotSource(ctx context.Context, dst *os.File, src io.Reader, remaining *int64) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(&contextReader{ctx: ctx, r: src}, *remaining+1))
	if err != nil {
		return "", 0, err
	}
	if n > *remaining {
		return "", 0, fmt.Errorf("family exceeds aggregate source limit")
	}
	*remaining -= n
	if err := dst.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func hashDescriptor(ctx context.Context, src io.Reader, maximum int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(&contextReader{ctx: ctx, r: src}, maximum+1))
	if err != nil {
		return "", err
	}
	if n > maximum {
		return "", &parser.ErrSourceTooLarge{}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func identityForFile(f *os.File) (fileIdentity, error) {
	identity, err := identityFromFile(f)
	if err != nil {
		return fileIdentity{}, err
	}
	return identity, nil
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

func validateSnapshotInputs(inputs []SnapshotInput) error {
	if len(inputs) == 0 || len(inputs) > session.MaxFamilySources {
		return errors.New("invalid family source set")
	}
	if inputs[0].Role != "main" || inputs[0].AgentID != "" {
		return errors.New("family must start with one main source")
	}
	seen := make(map[string]bool, len(inputs))
	for i, input := range inputs {
		if (i == 0 && input.Role != "main") || (i > 0 && input.Role != "child") {
			return errors.New("invalid family source role")
		}
		if i > 0 && (input.AgentID == "" || seen[input.AgentID]) {
			return errors.New("invalid child agent ID")
		}
		if i > 0 {
			seen[input.AgentID] = true
		}
	}
	return nil
}

func snapshotTestPaths(snapshot *FamilySnapshot) []string {
	if snapshot == nil {
		return nil
	}
	paths := []string{snapshot.dir}
	for _, source := range snapshot.Sources {
		paths = append(paths, source.path)
	}
	return paths
}
