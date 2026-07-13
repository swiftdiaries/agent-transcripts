package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

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
	s := NewS3(fake, "bucket", "prod")
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
	got.Metadata.Title = "new"
	if _, err := s.UpdateMetadata(context.Background(), p.ID, got.Metadata.Revision, got.Metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateMetadata(context.Background(), p.ID, got.Metadata.Revision, got.Metadata); !errors.Is(err, ErrConflict) {
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

type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string]S3Object
	puts     []string
	sequence int
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string]S3Object{}} }
func (f *fakeS3) GetObject(_ context.Context, _, key string) (S3Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	if !ok {
		return S3Object{}, ErrS3NotFound
	}
	o.Body = append([]byte(nil), o.Body...)
	return o, nil
}
func (f *fakeS3) HeadObject(ctx context.Context, bucket, key string) (S3Object, error) {
	return f.GetObject(ctx, bucket, key)
}
func (f *fakeS3) PutObject(_ context.Context, _, key string, body []byte, c S3Condition) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old, exists := f.objects[key]
	if c.IfNoneMatch && exists || c.IfMatch != "" && (!exists || old.ETag != c.IfMatch) {
		return "", ErrS3PreconditionFailed
	}
	f.sequence++
	etag := "etag-" + string(rune(f.sequence))
	f.objects[key] = S3Object{Body: append([]byte(nil), body...), ETag: etag}
	f.puts = append(f.puts, key)
	return etag, nil
}
func (f *fakeS3) CopyObject(ctx context.Context, bucket, src, dst string, c S3Condition) (string, error) {
	o, err := f.GetObject(ctx, bucket, src)
	if err != nil {
		return "", err
	}
	return f.PutObject(ctx, bucket, dst, o.Body, c)
}
func (f *fakeS3) DeleteObject(_ context.Context, _, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; !ok {
		return ErrS3NotFound
	}
	delete(f.objects, key)
	return nil
}
func (f *fakeS3) ListObjectsV2(_ context.Context, _, prefix, token string) (S3ListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
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
