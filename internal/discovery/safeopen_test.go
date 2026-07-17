package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func TestSnapshotFamilyRejectsIntermediateSymlinkReplacement(t *testing.T) {
	roots, family, replace := symlinkReplacementFamily(t)
	replace()
	if _, err := SnapshotFamily(context.Background(), family); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err=%v roots=%#v", err, roots)
	}
}

func TestSnapshotFamilyRejectsSameSizeInPlaceMutation(t *testing.T) {
	family, mutateBetweenPasses := mutatingFamily(t)
	snapshotTestHook = mutateBetweenPasses
	t.Cleanup(func() { snapshotTestHook = nil })
	if _, err := SnapshotFamily(context.Background(), family); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err=%v", err)
	}
}

func TestSnapshotFamilyFailureClosesDestinationBeforeCleanup(t *testing.T) {
	family, mutateBetweenPasses := mutatingFamily(t)
	var destination *os.File
	oldOpen := snapshotOpenFile
	snapshotOpenFile = func(dir string, index int) (*os.File, error) {
		file, err := snapshotFile(dir, index)
		destination = file
		return file, err
	}
	t.Cleanup(func() { snapshotOpenFile = oldOpen })
	oldRemoveAll := snapshotRemoveAll
	snapshotRemoveAll = func(dir string) error {
		if destination != nil {
			if _, err := destination.Stat(); err == nil {
				return errors.New("destination snapshot file is still open")
			}
		}
		return os.RemoveAll(dir)
	}
	t.Cleanup(func() { snapshotRemoveAll = oldRemoveAll })
	snapshotTestHook = mutateBetweenPasses
	t.Cleanup(func() { snapshotTestHook = nil })

	if _, err := SnapshotFamily(context.Background(), family); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err=%v", err)
	}
	if destination == nil {
		t.Fatal("snapshot destination was not created")
	}
	if _, err := os.Stat(filepath.Dir(destination.Name())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private snapshot directory remains after failure: %v", err)
	}
}

func TestFamilySnapshotCloseRemovesPrivateFiles(t *testing.T) {
	snapshot, err := SnapshotFamily(context.Background(), stableFamily(t))
	if err != nil {
		t.Fatal(err)
	}
	paths := snapshotTestPaths(snapshot)
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("path remains: %s", path)
		}
	}
}

func TestSnapshotReadersRejectsTooManySourcesAndCancellation(t *testing.T) {
	inputs := make([]SnapshotInput, session.MaxFamilySources+1)
	for i := range inputs {
		inputs[i] = SnapshotInput{Role: "child", AgentID: fmt.Sprintf("a-%d", i), Reader: strings.NewReader("{}\n")}
	}
	inputs[0].Role, inputs[0].AgentID = "main", ""
	if _, err := SnapshotReaders(context.Background(), SessionFamilyCandidate{}, inputs); err == nil {
		t.Fatal("accepted too many sources")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SnapshotReaders(ctx, SessionFamilyCandidate{}, inputs[:1]); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestDiscoverPropagatesUnsupportedSafeOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "done.jsonl")
	writeSession(t, path, snapshotFixture("unsupported"), 10*time.Minute)
	restore := setSafeOpenForTest(func(string, string) (*os.File, fileIdentity, error) {
		return nil, fileIdentity{}, ErrSafeOpenUnsupported
	})
	t.Cleanup(restore)
	_, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if !errors.Is(err, ErrSafeOpenUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestInspectPathPropagatesUnsupportedSafeOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "done.jsonl")
	writeSession(t, path, snapshotFixture("unsupported"), 10*time.Minute)
	restore := setSafeOpenForTest(func(string, string) (*os.File, fileIdentity, error) {
		return nil, fileIdentity{}, ErrSafeOpenUnsupported
	})
	t.Cleanup(restore)
	_, err := InspectPath(context.Background(), path, fixedNow, 5*time.Minute)
	if !errors.Is(err, ErrSafeOpenUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestInspectPathPropagatesSourceChangedSafeOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "done.jsonl")
	writeSession(t, path, snapshotFixture("changed"), 10*time.Minute)
	restore := setSafeOpenForTest(func(string, string) (*os.File, fileIdentity, error) {
		return nil, fileIdentity{}, ErrSourceChanged
	})
	t.Cleanup(restore)

	_, err := InspectPath(context.Background(), path, fixedNow, 5*time.Minute)
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err=%v", err)
	}
}

func stableFamily(t *testing.T) SessionFamilyCandidate {
	t.Helper()
	path := filepath.Join(t.TempDir(), "done.jsonl")
	writeSession(t, path, snapshotFixture("stable"), 10*time.Minute)
	candidate, err := InspectPath(context.Background(), path, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return SessionFamilyCandidate{Main: SourceCandidate{Candidate: candidate}}
}

func mutatingFamily(t *testing.T) (SessionFamilyCandidate, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "done.jsonl")
	original := snapshotFixture("mutate")
	writeSession(t, path, original, 10*time.Minute)
	candidate, err := InspectPath(context.Background(), path, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return SessionFamilyCandidate{Main: SourceCandidate{Candidate: candidate}}, func() {
		mutated := strings.Replace(original, "mutate", "change", 1)
		if len(mutated) != len(original) {
			t.Fatal("fixture mutation changed size")
		}
		if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func symlinkReplacementFamily(t *testing.T) (Roots, SessionFamilyCandidate, func()) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "records")
	path := filepath.Join(dir, "done.jsonl")
	writeSession(t, path, snapshotFixture("link"), 10*time.Minute)
	candidate, err := InspectPath(context.Background(), path, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeSession(t, filepath.Join(outside, "done.jsonl"), snapshotFixture("link"), 10*time.Minute)
	return Roots{Claude: []string{root}}, SessionFamilyCandidate{Main: SourceCandidate{Candidate: candidate}}, func() {
		if err := os.Rename(dir, filepath.Join(root, "records-old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, dir); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotFixture(id string) string {
	return `{"type":"user","sessionId":"` + id + `","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}
{"type":"system","subtype":"turn_duration","sessionId":"` + id + `","timestamp":"2026-07-12T10:00:01Z"}`
}
