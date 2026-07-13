package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func TestS3WritesManifestLast(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod/")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	keys := fake.putKeys()
	if keys[len(keys)-1] != "prod/users/ada/"+p.ID+"/manifest.json" {
		t.Fatalf("last key = %q", keys[len(keys)-1])
	}
}

func TestS3ConcurrentPutConverges(t *testing.T) {
	fake := newFakeS3()
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	start := make(chan struct{})
	results := make(chan struct {
		created bool
		err     error
	}, 2)
	for range 2 {
		go func() {
			<-start
			created, err := NewS3(fake, "bucket", "prod").PutSession(context.Background(), p)
			results <- struct {
				created bool
				err     error
			}{created, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.created == b.created {
		t.Fatalf("results = %+v %+v", a, b)
	}
	got, err := NewS3(fake, "bucket", "prod").ListSessions(context.Background(), p.Metadata.Destination)
	if err != nil || len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("listed = %+v, %v", got, err)
	}
}

func TestS3ListsOnlyFinalizedPackagesAcrossPages(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod").(*S3)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	orphanID := "s_" + strings.Repeat("0", 64)
	if _, err := fake.PutObject(context.Background(), "bucket", "prod/users/ada/"+orphanID+"/metadata.json", []byte("{}"), S3Condition{}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListSessions(context.Background(), p.Metadata.Destination); err != nil || len(got) != 0 {
		t.Fatalf("orphan list = %+v, %v", got, err)
	}
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListSessions(context.Background(), p.Metadata.Destination); err != nil || len(got) != 1 {
		t.Fatalf("list = %+v, %v", got, err)
	}
}

func TestS3MetadataUsesETagCAS(t *testing.T) {
	s := NewS3(newFakeS3(), "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := got.Metadata.Revision
	got.Metadata.Title = "new"
	if _, err := s.UpdateMetadata(context.Background(), p.ID, expected, got.Metadata); err != nil {
		t.Fatal(err)
	}
	got.Metadata.Title = "different"
	if _, err := s.UpdateMetadata(context.Background(), p.ID, expected, got.Metadata); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update = %v", err)
	}
}

func TestS3CreateProjectListsEmptyProject(t *testing.T) {
	s := NewS3(newFakeS3(), "bucket", "prod")
	if err := s.CreateProject(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	dirs, err := s.ListDirectories(context.Background(), "projects")
	if err != nil || len(dirs) != 1 || dirs[0].Slug != "demo" {
		t.Fatalf("directories = %+v, %v", dirs, err)
	}
}

func TestS3MoveToCurrentDestinationIsOwnershipCheckedNoOp(t *testing.T) {
	s := NewS3(newFakeS3(), "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := s.MoveSession(context.Background(), p.ID, "ada", p.Metadata.Destination, currentRevision(t, s, p.ID))
	if err != nil || got.ID != p.ID {
		t.Fatalf("move = %+v, %v", got, err)
	}
	if _, err := s.GetSession(context.Background(), p.ID); err != nil {
		t.Fatalf("source removed: %v", err)
	}
	if _, err := s.MoveSession(context.Background(), p.ID, "other", p.Metadata.Destination, currentRevision(t, s, p.ID)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ownership error = %v", err)
	}
}

func TestS3UpdateRetriesAfterCommittedManifestResponseLoss(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := got.Metadata.Revision
	got.Metadata.Title = "recovered"
	fake.failAfterPut("prod/users/ada/"+p.ID+"/manifest.json", errors.New("response lost"))
	if _, err := s.UpdateMetadata(context.Background(), p.ID, expected, got.Metadata); err != nil {
		t.Fatalf("response-loss convergence = %v", err)
	}
	if _, err := s.UpdateMetadata(context.Background(), p.ID, expected, got.Metadata); err != nil {
		t.Fatalf("retry = %v", err)
	}
}

func TestS3MoveRetriesAfterCommittedSourceManifestDeleteResponseLoss(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dest := session.Directory{Kind: "projects", Slug: "demo"}
	rev := currentRevision(t, s, p.ID)
	fake.failAfterDelete("prod/users/ada/"+p.ID+"/manifest.json", errors.New("response lost"))
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); err == nil {
		t.Fatal("wanted response-loss error")
	}
	got, err := s.MoveSession(context.Background(), p.ID, "ada", dest, rev)
	if err != nil {
		t.Fatalf("retry = %v", err)
	}
	if got.ID != session.PackageID(p.ContentID, dest) {
		t.Fatalf("destination id = %q", got.ID)
	}
	if _, err := s.GetSession(context.Background(), p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source = %v", err)
	}
	if _, err := s.GetSession(context.Background(), got.ID); err != nil {
		t.Fatalf("destination = %v", err)
	}
	if _, err := fake.GetObject(context.Background(), "bucket", "prod/.moves/"+p.ID+".json"); !errors.Is(err, ErrS3NotFound) {
		t.Fatalf("move marker = %v", err)
	}
	// Immutable source objects intentionally remain as non-visible orphans so a concurrent re-put cannot lose data.
}

func TestS3MoveRetriesAfterPreFinalizationMetadataResponseLoss(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dest := session.Directory{Kind: "projects", Slug: "demo"}
	newID := session.PackageID(p.ContentID, dest)
	fake.failAfterPut("prod/projects/demo/"+newID+"/metadata.json", errors.New("response lost"))
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); err == nil {
		t.Fatal("wanted response-loss error")
	}
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); err != nil {
		t.Fatalf("retry = %v", err)
	}
}

func TestFakeS3ListingIsBucketIsolated(t *testing.T) {
	fake := newFakeS3()
	_, _ = fake.PutObject(context.Background(), "a", "prod/users/ada/x", []byte("a"), S3Condition{})
	_, _ = fake.PutObject(context.Background(), "b", "prod/users/ada/y", []byte("b"), S3Condition{})
	page, err := fake.ListObjectsV2(context.Background(), "a", "prod/", "")
	if err != nil || len(page.Keys) != 1 || page.Keys[0] != "prod/users/ada/x" {
		t.Fatalf("page = %+v, %v", page, err)
	}
}

func TestS3ListCancelsBetweenPages(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fake.cancelAfterFirstList = cancel
	_, err := s.ListSessions(ctx, p.Metadata.Destination)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestS3MoveDoesNotHideSourceAfterManifestETagRace(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dest := session.Directory{Kind: "projects", Slug: "demo"}
	newID := session.PackageID(p.ContentID, dest)
	sourceKey := fakeKey("bucket", "prod/users/ada/"+p.ID+"/manifest.json")
	fake.setOnPut("prod/projects/demo/"+newID+"/manifest.json", func() {
		o := fake.objects[sourceKey]
		fake.sequence++
		o.ETag = "etag-" + string(rune(fake.sequence))
		fake.objects[sourceKey] = o
	})
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); !errors.Is(err, ErrConflict) {
		t.Fatalf("move = %v", err)
	}
	if _, err := s.GetSession(context.Background(), p.ID); err != nil {
		t.Fatalf("source lost: %v", err)
	}
}

func TestS3MoveReconcilesDestinationWhenSourceDeleteWins(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dest := session.Directory{Kind: "projects", Slug: "demo"}
	newID := session.PackageID(p.ContentID, dest)
	fake.setOnPut("prod/projects/demo/"+newID+"/manifest.json", func() { delete(fake.objects, fakeKey("bucket", "prod/users/ada/"+p.ID+"/manifest.json")) })
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); !errors.Is(err, ErrConflict) {
		t.Fatalf("move = %v", err)
	}
	if _, err := s.GetSession(context.Background(), newID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("destination visible = %v", err)
	}
}

func TestS3UpdateMetadataConflictsWithMoveLockedManifest(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod").(*S3)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	m, etag, err := s.readManifest(context.Background(), p.Metadata.Destination, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.MoveID = p.ID
	key, _ := s.key(p.Metadata.Destination, p.ID, "manifest.json")
	if _, err := fake.PutObject(context.Background(), "bucket", key, mustJSON(m), S3Condition{IfMatch: etag}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Metadata.Title = "blocked"
	if _, err := s.UpdateMetadata(context.Background(), p.ID, got.Metadata.Revision, got.Metadata); !errors.Is(err, ErrConflict) {
		t.Fatalf("update = %v", err)
	}
	if _, err := s.GetSession(context.Background(), p.ID); err != nil {
		t.Fatalf("locked source unreadable: %v", err)
	}
}

func TestS3MoveRetryPreservesSourceRePutAfterAmbiguousDelete(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	dest := session.Directory{Kind: "projects", Slug: "demo"}
	fake.failAfterDelete("prod/users/ada/"+p.ID+"/manifest.json", errors.New("response lost"))
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); err == nil {
		t.Fatal("wanted response loss")
	}
	if created, err := s.PutSession(context.Background(), p); err != nil || !created {
		t.Fatalf("reput = %v, %v", created, err)
	}
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry = %v", err)
	}
	if _, err := s.GetSession(context.Background(), p.ID); err != nil {
		t.Fatalf("reput source = %v", err)
	}
}

func TestS3RePutReclaimsInvisibleMetadataOrphan(t *testing.T) {
	s := NewS3(newFakeS3(), "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(context.Background(), p.ID, "ada", currentRevision(t, s, p.ID)); err != nil {
		t.Fatal(err)
	}
	reput := p
	reput.Metadata.Title = "new title"
	reput.Metadata.Tags = []string{"new"}
	reput.Metadata.UploadedAt = time.Now().UTC()
	reput.Metadata.Revision = ""
	created, err := s.PutSession(context.Background(), reput)
	if err != nil || !created {
		t.Fatalf("reput = %v, %v", created, err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil || got.Metadata.Title != "new title" || len(got.Metadata.Tags) != 1 {
		t.Fatalf("got = %+v, %v", got.Metadata, err)
	}
}

func TestS3ReclaimClaimDoesNotCorruptConcurrentManifestWinner(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod").(*S3)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	winner, _, err := s.readManifest(context.Background(), p.Metadata.Destination, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(context.Background(), p.ID, "ada", currentRevision(t, s, p.ID)); err != nil {
		t.Fatal(err)
	}
	fake.setOnPut("prod/users/ada/"+p.ID+"/.reclaim", func() {
		fake.sequence++
		fake.objects[fakeKey("bucket", "prod/users/ada/"+p.ID+"/manifest.json")] = S3Object{Body: mustJSON(winner), ETag: "etag-" + string(rune(fake.sequence))}
	})
	reput := p
	reput.Metadata.Title = "loser"
	if _, err := s.PutSession(context.Background(), reput); !errors.Is(err, ErrConflict) {
		t.Fatalf("reclaim = %v", err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil || got.Metadata.Title != "" {
		t.Fatalf("winner = %+v,%v", got.Metadata, err)
	}
}

func TestS3ReclaimClaimResponseLossRetryConverges(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(context.Background(), p.ID, "ada", currentRevision(t, s, p.ID)); err != nil {
		t.Fatal(err)
	}
	reput := p
	reput.Metadata.Title = "retry"
	fake.failAfterPut("prod/users/ada/"+p.ID+"/.reclaim", errors.New("response lost"))
	if _, err := s.PutSession(context.Background(), reput); err == nil {
		t.Fatal("wanted response loss")
	}
	if created, err := s.PutSession(context.Background(), reput); err != nil || !created {
		t.Fatalf("retry = %v,%v", created, err)
	}
}

func TestS3ReclaimClaimDeleteResponseLossDoesNotBreakIdempotency(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(context.Background(), p.ID, "ada", currentRevision(t, s, p.ID)); err != nil {
		t.Fatal(err)
	}
	reput := p
	reput.Metadata.Title = "retry"
	fake.failAfterDelete("prod/users/ada/"+p.ID+"/.reclaim", errors.New("response lost"))
	if _, err := s.PutSession(context.Background(), reput); err != nil {
		t.Fatal(err)
	}
	if created, err := s.PutSession(context.Background(), reput); err != nil || created {
		t.Fatalf("idempotent retry = %v,%v", created, err)
	}
}

func TestS3ConcurrentIdenticalReclaimersConverge(t *testing.T) {
	fake := newFakeS3()
	s := NewS3(fake, "bucket", "prod")
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(context.Background(), p.ID, "ada", currentRevision(t, s, p.ID)); err != nil {
		t.Fatal(err)
	}
	reput := p
	reput.Metadata.Title = "reclaimed"
	metadataKey := "prod/users/ada/" + p.ID + "/metadata.json"
	reclaimKey := "prod/users/ada/" + p.ID + "/.reclaim"
	manifestKey := "prod/users/ada/" + p.ID + "/manifest.json"
	metadataReplaced := make(chan struct{})
	reclaimObserved := make(chan struct{})
	manifestWritten := make(chan struct{})
	releaseWinner := make(chan struct{})
	releaseLoser := make(chan struct{})
	fake.setAfterPut(metadataKey, func() {
		close(metadataReplaced)
		<-releaseWinner
	})
	fake.setAfterHead(reclaimKey, func() {
		close(reclaimObserved)
		<-releaseLoser
	})
	fake.setAfterPut(manifestKey, func() { close(manifestWritten) })
	results := make(chan struct {
		created bool
		err     error
	}, 2)
	go func() {
		created, err := s.PutSession(context.Background(), reput)
		results <- struct {
			created bool
			err     error
		}{created, err}
	}()
	waitForSignal(t, metadataReplaced, "winner metadata replacement")
	go func() {
		created, err := s.PutSession(context.Background(), reput)
		results <- struct {
			created bool
			err     error
		}{created, err}
	}()
	waitForSignal(t, reclaimObserved, "loser reclaim observation")
	close(releaseWinner)
	waitForSignal(t, manifestWritten, "winner manifest")
	close(releaseLoser)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.created == b.created {
		t.Fatalf("results=%+v %+v", a, b)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil || got.Metadata.Title != "reclaimed" {
		t.Fatalf("got=%+v,%v", got.Metadata, err)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

type fakeS3 struct {
	mu                   sync.Mutex
	objects              map[string]S3Object
	puts                 []string
	sequence             int
	afterPut             map[string]error
	afterDelete          map[string]error
	cancelAfterFirstList func()
	listCalls            int
	onPut                map[string]func()
	afterPutHook         map[string]func()
	afterHeadHook        map[string]func()
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string]S3Object{}, afterPut: map[string]error{}, afterDelete: map[string]error{}, onPut: map[string]func(){}, afterPutHook: map[string]func(){}, afterHeadHook: map[string]func(){}}
}
func fakeKey(bucket, key string) string { return bucket + "\x00" + key }
func (f *fakeS3) GetObject(_ context.Context, bucket, key string) (S3Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[fakeKey(bucket, key)]
	if !ok {
		return S3Object{}, ErrS3NotFound
	}
	o.Body = append([]byte(nil), o.Body...)
	return o, nil
}
func (f *fakeS3) HeadObject(_ context.Context, bucket, key string) (S3Object, error) {
	f.mu.Lock()
	o, ok := f.objects[fakeKey(bucket, key)]
	hook := f.afterHeadHook[key]
	delete(f.afterHeadHook, key)
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if !ok {
		return S3Object{}, ErrS3NotFound
	}
	o.Body = append([]byte(nil), o.Body...)
	return o, nil
}
func (f *fakeS3) PutObject(_ context.Context, bucket, key string, body []byte, c S3Condition) (string, error) {
	f.mu.Lock()
	objectKey := fakeKey(bucket, key)
	old, exists := f.objects[objectKey]
	if c.IfNoneMatch && exists || c.IfMatch != "" && (!exists || old.ETag != c.IfMatch) {
		f.mu.Unlock()
		return "", ErrS3PreconditionFailed
	}
	f.sequence++
	etag := "etag-" + string(rune(f.sequence))
	f.objects[objectKey] = S3Object{Body: append([]byte(nil), body...), ETag: etag}
	f.puts = append(f.puts, key)
	if hook := f.onPut[key]; hook != nil {
		delete(f.onPut, key)
		hook()
	}
	afterHook := f.afterPutHook[key]
	delete(f.afterPutHook, key)
	if err := f.afterPut[key]; err != nil {
		delete(f.afterPut, key)
		f.mu.Unlock()
		return "", err
	}
	f.mu.Unlock()
	if afterHook != nil {
		afterHook()
	}
	return etag, nil
}
func (f *fakeS3) failAfterPut(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterPut[key] = err
}
func (f *fakeS3) setOnPut(key string, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onPut[key] = hook
}
func (f *fakeS3) setAfterPut(key string, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterPutHook[key] = hook
}
func (f *fakeS3) setAfterHead(key string, hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterHeadHook[key] = hook
}
func (f *fakeS3) failAfterDelete(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterDelete[key] = err
}
func (f *fakeS3) CopyObject(_ context.Context, bucket, src, dst string, c S3Condition) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[fakeKey(bucket, src)]
	if !ok {
		return "", ErrS3NotFound
	}
	dstKey := fakeKey(bucket, dst)
	old, exists := f.objects[dstKey]
	if c.IfNoneMatch && exists || c.IfMatch != "" && (!exists || old.ETag != c.IfMatch) {
		return "", ErrS3PreconditionFailed
	}
	f.sequence++
	etag := "etag-" + string(rune(f.sequence))
	f.objects[dstKey] = S3Object{Body: append([]byte(nil), o.Body...), ETag: etag}
	return etag, nil
}
func (f *fakeS3) DeleteObject(_ context.Context, bucket, key string, c S3Condition) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	objectKey := fakeKey(bucket, key)
	o, ok := f.objects[objectKey]
	if !ok {
		return ErrS3NotFound
	}
	if c.IfMatch != "" && c.IfMatch != o.ETag {
		return ErrS3PreconditionFailed
	}
	delete(f.objects, objectKey)
	if err := f.afterDelete[key]; err != nil {
		delete(f.afterDelete, key)
		return err
	}
	return nil
}
func (f *fakeS3) ListObjectsV2(_ context.Context, bucket, prefix, token string) (S3ListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listCalls == 1 && f.cancelAfterFirstList != nil {
		f.cancelAfterFirstList()
	}
	var keys []string
	for objectKey := range f.objects {
		parts := strings.SplitN(objectKey, "\x00", 2)
		if len(parts) == 2 && parts[0] == bucket && strings.HasPrefix(parts[1], prefix) {
			keys = append(keys, parts[1])
		}
	}
	sort.Strings(keys)
	start := 0
	if token != "" {
		for start < len(keys) && keys[start] <= token {
			start++
		}
	}
	end := start + 2
	if end > len(keys) {
		end = len(keys)
	}
	page := S3ListPage{Keys: append([]string(nil), keys[start:end]...)}
	if end < len(keys) {
		page.NextToken = keys[end-1]
	}
	return page, nil
}
func (f *fakeS3) putKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.puts...)
}
