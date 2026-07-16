package publish

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

func TestClientUploadSendsMainThenSortedChildren(t *testing.T) {
	s := newLoopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(64 << 10); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		if got := len(r.MultipartForm.File["source"]); got != 1 {
			t.Fatalf("main files = %d", got)
		}
		children := r.MultipartForm.File["child"]
		if len(children) != 2 || children[0].Filename != "a.jsonl" || children[1].Filename != "z.jsonl" {
			t.Fatalf("children = %#v", children)
		}
		w.Header().Set("Location", "/sessions/s_test")
		w.WriteHeader(http.StatusCreated)
	}))
	defer s.Close()
	_, err := (Client{BaseURL: s.URL, Token: "short-lived"}).Upload(context.Background(), Request{
		SourceName: "main.jsonl", Source: bytes.NewBufferString("main"), Destination: "projects/platform",
		Children: []ChildSource{{SourceName: "z.jsonl", Source: bytes.NewBufferString("z")}, {SourceName: "a.jsonl", Source: bytes.NewBufferString("a")}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientUploadRejectsCrossOriginLocation(t *testing.T) {
	s := newLoopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer short-lived" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Location", "https://attacker.example/sessions/s_x")
		w.WriteHeader(http.StatusCreated)
	}))
	defer s.Close()

	_, err := (Client{BaseURL: s.URL, Token: "short-lived"}).Upload(context.Background(), Request{
		SourceName: "session.jsonl", Source: bytes.NewBufferString("source"), Destination: "projects/platform",
	})
	if err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("error = %v", err)
	}
}

func TestClientUploadErrorDoesNotExposeResponseBodyOrToken(t *testing.T) {
	secret := "bearer-secret-and-source-body"
	s := newLoopbackServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(secret))
	}))
	defer s.Close()
	_, err := (Client{BaseURL: s.URL, Token: secret}).Upload(context.Background(), Request{SourceName: "session.jsonl", Source: bytes.NewBufferString(secret), Destination: "projects/platform"})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
}

func newLoopbackServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit loopback test listener: %v", err)
		}
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	return server
}
