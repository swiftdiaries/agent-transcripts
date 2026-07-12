# Agent Transcripts v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a single Go binary that discovers, imports, renders, and shares completed Claude Code and Codex transcripts locally or through an authenticated hosted service.

**Architecture:** A server-rendered modular Go monolith owns one canonical session model. Provider parsers, storage backends, and identity providers sit behind narrow interfaces; CLI commands and HTTP handlers call the same application services. Raw JSONL and normalized JSON are stored together as immutable packages while catalog metadata remains mutable by the uploader.

**Tech Stack:** Go, standard `net/http`, `html/template`, `embed`, YAML v3, AWS SDK for Go v2 S3 client, `coreos/go-oidc`, and Go's standard testing package.

## Global Constraints

- Produce one `agent-transcripts` Go binary and one minimal container image.
- Use server-rendered HTML; JavaScript is progressive enhancement only.
- Support Claude Code and Codex JSONL, preserving unknown events as raw events.
- Import and upload completed sessions only; use a five-minute quiet period when no terminal marker exists.
- Persist `source.jsonl`, `normalized.json`, `metadata.json`, and a final visibility manifest.
- Support filesystem storage locally and filesystem or S3-compatible storage remotely.
- Support local, trusted-proxy-header, and OIDC identity modes selected by flags or YAML.
- All authenticated employees can browse and publish to projects; only the uploader can mutate a transcript.
- Store normalized free-form tags in v1 but do not implement tag filtering.
- Do not add a database, SPA, search index, queue, background sync, project ACLs, comments, or Kubernetes packaging.
- Never log transcript bodies, tool output, credentials, or authentication tokens.
- Treat a local file's quiet-period eligibility as local evidence only. A raw
  browser upload without a provider terminal marker is rejected because the
  hosted server cannot verify the source file's age or whether it is active.
- Derive a content ID from `provider + source checksum`; derive each managed
  package ID from `content ID + destination`. This makes retries to one
  destination idempotent while allowing the same source in different
  destinations, as required by the design.
- Cap raw sources at 64 MiB, individual JSONL records at 16 MiB, titles at 200
  UTF-8 bytes, descriptions at 4 KiB, 20 tags per transcript, and each tag at
  64 ASCII characters. Reject oversize input before parsing or allocation.
- Accept state-changing browser requests only with same-origin `Origin` (or
  `Referer` when `Origin` is absent) and a per-session CSRF token. API clients
  authenticate with a 15-minute audience-bound bearer token minted from an
  authenticated, CSRF-protected browser page; proxy identity headers alone are
  not accepted on API mutation routes exposed to browsers. Keep the signing
  key in an environment variable and never persist minted tokens.
- Normalize and validate user keys, directory kinds/slugs, package IDs, and
  route parameters before constructing filesystem paths or S3 keys.

## Planned File Structure

```text
cmd/agent-transcripts/main.go          command dispatch and process exit
internal/cli/commands.go               serve/import/upload/version commands
internal/config/config.go              YAML, environment, and flags
internal/session/model.go              canonical session and package types
internal/session/validate.go           canonical and metadata validation
internal/session/identity.go           content and destination package IDs
internal/parser/{parser,claude,codex}.go
internal/parser/testdata/              sanitized provider fixtures
internal/discovery/discovery.go        merged local catalog
internal/store/{store,filesystem,s3}.go
internal/library/service.go            import, checksum, and idempotency
internal/publish/client.go              hosted multipart client
internal/auth/{identity,proxy,oidc}.go
internal/auth/csrf.go                  browser mutation protection
internal/web/{server,handlers}.go
internal/web/templates/                embedded HTML templates
internal/web/static/                   embedded CSS and JavaScript
skills/publish-transcript/SKILL.md
.claude-plugin/{plugin,marketplace}.json
Dockerfile
README.md
config.example.yaml
```

---

### Task 1: Bootstrap the Binary and Configuration

**Files:**
- Create: `go.mod`
- Create: `cmd/agent-transcripts/main.go`
- Create: `internal/cli/commands.go`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Load(path string, overrides config.Overrides) (config.Config, error)`
- Produces: `cli.Run(ctx context.Context, args []string, stdout, stderr io.Writer) int`

- [ ] **Step 1: Write failing precedence and validation tests**

```go
func TestLoadAppliesDefaultsYAMLThenOverrides(t *testing.T) {
    path := writeTempConfig(t, "mode: hosted\nlisten: \":9000\"\nstorage:\n  type: filesystem\n  root: /yaml/library\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\n")
    got, err := Load(path, Overrides{Listen: ptr(":9100")})
    if err != nil { t.Fatal(err) }
    if got.Listen != ":9100" { t.Fatalf("listen = %q", got.Listen) }
    if got.Storage.Root != "/yaml/library" { t.Fatalf("root = %q", got.Storage.Root) }
    if got.QuietPeriod != 5*time.Minute { t.Fatalf("quiet = %s", got.QuietPeriod) }
}
func TestHostedRejectsLocalIdentity(t *testing.T) {
    _, err := Load(writeTempConfig(t, "mode: hosted\nauth:\n  type: local\n"), Overrides{})
    if err == nil || !strings.Contains(err.Error(), "hosted mode requires proxy or oidc auth") { t.Fatalf("error = %v", err) }
}
func TestHostedRequiresExternalOriginAndSessionKey(t *testing.T) {
    _, err := Load(writeTempConfig(t, "mode: hosted\nauth:\n  type: proxy\n"), Overrides{})
    if err == nil || !strings.Contains(err.Error(), "external_origin") { t.Fatalf("error = %v", err) }
}
```

- [ ] **Step 2: Run `go test ./internal/config -v`**

Expected: FAIL because configuration types and `Load` do not exist.

- [ ] **Step 3: Implement configuration and command dispatch**

Define `Config` with mode, listen, external origin, upload limits, storage, auth, trusted proxy CIDRs, source roots, and names of environment variables containing cookie/token keys. Apply defaults, strict YAML decoding, environment secrets, and explicit overrides in that order; reject secret values embedded in YAML. Hosted mode requires an HTTPS external origin outside tests, at least one current 32-byte key, and proxy CIDRs when proxy auth is selected. Validate mode/storage/auth combinations. Make `cli.Run` recognize only `serve`, `import`, `upload`, `version`, and `help`; unknown commands return 2.

- [ ] **Step 4: Run `go test ./internal/config ./internal/cli ./cmd/agent-transcripts`**

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd/agent-transcripts internal/cli internal/config
git commit -m "feat: bootstrap binary and configuration"
```

### Task 2: Define the Canonical Session Model

**Files:**
- Create: `internal/session/model.go`
- Create: `internal/session/validate.go`
- Create: `internal/session/identity.go`
- Test: `internal/session/model_test.go`

**Interfaces:**
- Produces: `session.Event`, `session.Session`, `session.Metadata`, and `session.Package`
- Produces: `session.Directory` and `session.SourceFacts{ObservedModTime, ObservedSize, QuietPeriodVerified}`
- Produces: `session.NormalizeTags([]string) ([]string, error)`
- Produces: `session.Validate(session.Session) error`
- Produces: `session.ContentID(provider, sourceChecksum string) string`
- Produces: `session.PackageID(contentID string, destination session.Directory) string`

- [ ] **Step 1: Write failing model tests**

```go
func TestNormalizeTags(t *testing.T) {
    got, err := NormalizeTags([]string{" Rust ", "frontend", "rust", "project-1123"})
    if err != nil { t.Fatal(err) }
    if !slices.Equal(got, []string{"rust", "frontend", "project-1123"}) { t.Fatalf("tags = %v", got) }
}
func TestValidateRawEvent(t *testing.T) {
    s := Session{SchemaVersion: 1, ID: "s_123", Provider: "claude", Events: []Event{{
        ID: "e_1", Kind: EventRaw, RawType: "future_event", Raw: json.RawMessage(`{"x":1}`),
    }}}
    if err := Validate(s); err != nil { t.Fatal(err) }
}
func TestPackageIDSeparatesDestinationsAndIsStable(t *testing.T) {
    content := ContentID("claude", strings.Repeat("a", 64))
    users := PackageID(content, Directory{Kind: "users", Slug: "ada"})
    project := PackageID(content, Directory{Kind: "projects", Slug: "platform"})
    if users == project { t.Fatal("destinations collided") }
    if users != PackageID(content, Directory{Kind: "users", Slug: "ada"}) { t.Fatal("ID is unstable") }
}
```

- [ ] **Step 2: Run `go test ./internal/session -v`**

Expected: FAIL because canonical types are undefined.

- [ ] **Step 3: Implement the types and validation**

```go
type EventKind string
const (
    EventUser EventKind = "user"
    EventAssistant EventKind = "assistant"
    EventToolCall EventKind = "tool_call"
    EventToolResult EventKind = "tool_result"
    EventFileChange EventKind = "file_change"
    EventCommit EventKind = "commit"
    EventError EventKind = "error"
    EventRaw EventKind = "raw"
)
type Event struct {
    ID, ParentID, AgentID string
    Kind EventKind
    Time time.Time
    Text, ToolName string
    Input, Output json.RawMessage
    RawType string
    Raw json.RawMessage
}
```

Add session identity/timestamps/project/events, all approved metadata fields, and raw/normalized/source package bytes. Define `Directory` in this package so identity and storage share one validated type. Validate required IDs, kinds, metadata lengths, directory kinds/slugs, and timestamps. Raw events require a type and valid JSON. Normalize tags by trimming, lowercasing, stable deduplication, and `[a-z0-9_-]+` validation. Use length-delimited hash inputs rather than string concatenation; IDs are `c_`/`s_` plus lowercase SHA-256 hex.

- [ ] **Step 4: Run `go test ./internal/session -v`**

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/session
git commit -m "feat: define canonical transcript model"
```

### Task 3: Parse Claude and Codex Fixtures

**Files:**
- Create: `internal/parser/parser.go`
- Create: `internal/parser/claude.go`
- Create: `internal/parser/codex.go`
- Test: `internal/parser/parser_test.go`
- Create: `internal/parser/testdata/claude-session.jsonl`
- Create: `internal/parser/testdata/codex-session.jsonl`

**Interfaces:**
- Consumes: canonical session types
- Produces: `parser.Registry.DetectAndParse(ctx context.Context, source io.Reader) (session.Session, error)`
- Produces: `session.Session.Completion` with `Terminal`, `TerminalReason`, and
  `LastEventAt`; parsers set terminal evidence only from documented provider
  records, never from EOF.

- [ ] **Step 1: Add sanitized fixtures and failing parser tests**

Each fixture contains user/assistant messages, a tool call/result, and one unknown event.

```go
func TestRegistryParsesFixtures(t *testing.T) {
    for _, tt := range []struct{ file, provider string }{{"testdata/claude-session.jsonl", "claude"}, {"testdata/codex-session.jsonl", "codex"}} {
        t.Run(tt.provider, func(t *testing.T) {
            f, err := os.Open(tt.file); if err != nil { t.Fatal(err) }; defer f.Close()
            got, err := DefaultRegistry().DetectAndParse(context.Background(), f)
            if err != nil { t.Fatal(err) }
            if got.Provider != tt.provider { t.Fatalf("provider = %q", got.Provider) }
            if countKind(got.Events, session.EventRaw) != 1 { t.Fatal("unknown event not preserved") }
        })
    }
}
func TestParserDoesNotTreatEOFAsTerminal(t *testing.T) {
    got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(incompleteClaudeJSONL))
    if err != nil { t.Fatal(err) }
    if got.Completion.Terminal { t.Fatal("EOF incorrectly treated as completion") }
}
```

- [ ] **Step 2: Run `go test ./internal/parser -v`**

Expected: FAIL because registry and parsers do not exist.

- [ ] **Step 3: Implement streaming JSONL detection and normalization**

```go
type Parser interface {
    Provider() string
    Detect(first json.RawMessage) bool
    Parse(ctx context.Context, lines []json.RawMessage) (session.Session, error)
}
type Registry struct { parsers []Parser }
```

Wrap the input in a 64 MiB limiting reader and use a scanner with a 16 MiB token limit. Return typed `ErrSourceTooLarge` and `ErrRecordTooLarge` errors without including record content. Reject malformed JSON. Detect from the first meaningful line, map common events, and copy every unmapped line into `EventRaw`. Use provider event IDs when present and line-number IDs otherwise. Record provider-specific terminal evidence separately from event mapping and cover both terminal and non-terminal fixtures.

- [ ] **Step 4: Run `go test ./internal/parser ./internal/session -v`**

Expected: PASS with one raw event per fixture.

- [ ] **Step 5: Commit**

```bash
git add internal/parser
git commit -m "feat: parse Claude and Codex sessions"
```

### Task 4: Discover Completed Local Sessions

**Files:**
- Create: `internal/discovery/discovery.go`
- Test: `internal/discovery/discovery_test.go`
- Modify: `internal/cli/commands.go`

**Interfaces:**
- Produces: `discovery.Discover(ctx context.Context, roots Roots, now time.Time, quiet time.Duration) ([]Candidate, error)`
- Produces: `Candidate{Path, Provider, SessionID, Project, Title, StartedAt, Status}`
- Produces: `discovery.OpenEligible(candidate Candidate) (io.ReadCloser, session.SourceFacts, error)`,
  which opens once, stats the opened descriptor, and rechecks completion before
  import to avoid a discovery/import time-of-check-to-time-of-use race.

- [ ] **Step 1: Write failing merge and quiet-period tests**

```go
func TestDiscoverMergesNewestFirst(t *testing.T) {
    got, err := Discover(context.Background(), fixtureRoots(t), fixedNow, 5*time.Minute)
    if err != nil { t.Fatal(err) }
    if len(got) != 2 || got[0].StartedAt.Before(got[1].StartedAt) { t.Fatalf("got %#v", got) }
}
func TestDiscoverHidesFileInsideQuietPeriod(t *testing.T) {
    got, err := Discover(context.Background(), rootsWithAge(t, 2*time.Minute), fixedNow, 5*time.Minute)
    if err != nil { t.Fatal(err) }
    if len(got) != 0 { t.Fatalf("active candidates = %d", len(got)) }
}
func TestOpenEligibleRejectsFileChangedAfterDiscovery(t *testing.T) {
    candidate := discoverOne(t, rootsWithAge(t, 10*time.Minute))
    appendToFile(t, candidate.Path, []byte("\n{}"))
    if _, _, err := OpenEligible(candidate); !errors.Is(err, ErrSourceChanged) { t.Fatalf("error = %v", err) }
}
```

- [ ] **Step 2: Run `go test ./internal/discovery -v`**

Expected: FAIL because discovery is undefined.

- [ ] **Step 3: Implement discovery and CLI selection**

Walk configured roots without following symlinks, accept provider filename patterns, extract cheap metadata, hide setup-only/malformed/active sessions, and sort by start time then path. Eligibility is terminal evidence or unchanged `mtime+size` beyond the quiet period; capture those facts in `Candidate` and recheck them on the same descriptor used for import. A terminal picker prints numbered merged choices and accepts comma-separated selections. Non-interactive use requires a path or `--latest`. Add `--provider` and `--limit`. Explicit paths go through the same eligibility check and cannot bypass completion filtering.

- [ ] **Step 4: Run `go test ./internal/discovery ./internal/cli -v`**

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/discovery internal/cli/commands.go
git commit -m "feat: discover completed local sessions"
```

### Task 5: Store and Import Filesystem Packages

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/filesystem.go`
- Test: `internal/store/store_test.go`
- Create: `internal/library/service.go`
- Test: `internal/library/service_test.go`
- Modify: `internal/cli/commands.go`

**Interfaces:**
- Produces: `store.Store`
- Produces: `library.Service.Import(ctx context.Context, source io.Reader, facts session.SourceFacts, attrs ImportAttrs) (session.Metadata, error)`

- [ ] **Step 1: Write failing visibility and idempotency tests**

```go
func TestFilesystemListsOnlyFinalizedPackages(t *testing.T) {
    s := NewFilesystem(t.TempDir())
    writeIncompletePackage(t, s.root, "s_incomplete")
    got, err := s.ListSessions(context.Background(), session.Directory{Kind: "users", Slug: "ada"})
    if err != nil { t.Fatal(err) }
    if len(got) != 0 { t.Fatalf("listed incomplete package: %v", got) }
}
func TestImportIsIdempotent(t *testing.T) {
    svc := newTestService(t)
    facts := session.SourceFacts{QuietPeriodVerified: true}
    first, _ := svc.Import(ctx, fixture("claude-session.jsonl"), facts, attrs)
    second, _ := svc.Import(ctx, fixture("claude-session.jsonl"), facts, attrs)
    if first.ID != second.ID { t.Fatalf("ids differ: %s %s", first.ID, second.ID) }
}
func TestImportRejectsUnprovenCompletion(t *testing.T) {
    _, err := newTestService(t).Import(ctx, fixture("incomplete-claude.jsonl"), session.SourceFacts{}, attrs)
    if !errors.Is(err, library.ErrIncomplete) { t.Fatalf("error = %v", err) }
}
```

- [ ] **Step 2: Run `go test ./internal/store ./internal/library -v`**

Expected: FAIL because store and service do not exist.

- [ ] **Step 3: Implement store and import service**

```go
type Store interface {
    ListDirectories(context.Context, string) ([]session.Directory, error)
    CreateProject(context.Context, string) error
    ListSessions(context.Context, session.Directory) ([]session.Metadata, error)
    GetSession(context.Context, string) (session.Package, error)
    PutSession(context.Context, session.Package) (created bool, err error)
    UpdateMetadata(context.Context, string, expectedRevision string, session.Metadata) (newRevision string, err error)
    MoveSession(context.Context, string, string, session.Directory) (session.Metadata, error)
    DeleteSession(context.Context, string, string) error
}
```

Write package files into a same-filesystem temporary directory, `fsync` files and directory, rename it, then atomically create `manifest.json` last. The manifest contains package ID, content ID, immutable-file hashes, metadata revision, and schema version. `PutSession` is create-if-absent: an existing manifest with identical content/destination returns `created=false`; mismatched content returns `ErrConflict`. Metadata writes use revision compare-and-swap so concurrent edits cannot silently overwrite each other. Validate every logical identifier before joining a path and reject symlinked package components. Import verifies terminal evidence or trusted local quiet-period `session.SourceFacts`, hashes while reading a bounded temporary snapshot, parses that exact snapshot, derives content/package IDs, validates, and calls `PutSession`. Wire CLI import through `OpenEligible`; never reopen the source path.

- [ ] **Step 4: Run `go test ./internal/store ./internal/library ./internal/cli -v`**

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/library internal/cli/commands.go
git commit -m "feat: import sessions into filesystem library"
```

### Task 6: Add S3-Compatible Storage

**Files:**
- Create: `internal/store/s3.go`
- Test: `internal/store/s3_test.go`
- Modify: `internal/config/config.go`

**Interfaces:**
- Consumes: `store.Store`
- Produces: `store.NewS3(client S3API, bucket, prefix string) Store`

- [ ] **Step 1: Write a failing fake-S3 contract test**

```go
func TestS3WritesManifestLast(t *testing.T) {
    fake := newFakeS3()
    s := NewS3(fake, "bucket", "prod/")
    if err := s.PutSession(ctx, fixturePackage()); err != nil { t.Fatal(err) }
    keys := fake.putKeys()
    if keys[len(keys)-1] != "prod/sessions/s_123/manifest.json" { t.Fatalf("last key = %q", keys[len(keys)-1]) }
}
func TestStoreContractConcurrentPutConverges(t *testing.T) {
    created := runConcurrentIdenticalPuts(t, storeFactory)
    if created != 1 { t.Fatalf("created count = %d", created) }
    assertOneVisibleValidPackage(t, storeFactory)
}
```

Run the common store contract suite against filesystem and fake S3.

- [ ] **Step 2: Run `go test ./internal/store -run 'TestS3|TestStoreContract' -v`**

Expected: FAIL because `NewS3` is undefined.

- [ ] **Step 3: Implement prefix listing and manifest-last writes**

Define the smallest client interface around GetObject, conditional PutObject, DeleteObject, CopyObject, HeadObject, and paginated ListObjectsV2. Map validated logical paths beneath the configured prefix. Finalize with conditional manifest creation (`If-None-Match: *`); on precondition failure, read and compare the winner's manifest. Metadata compare-and-swap uses object ETags. Move copies immutable objects to the destination package ID, conditionally finalizes the destination, then removes the source manifest before best-effort source cleanup; retries converge on one visible destination. Listing treats manifests as the sole visibility boundary and tolerates orphaned non-manifest objects. Build the real AWS client only in composition code; tests use a concurrency-safe fake that models preconditions and pagination.

- [ ] **Step 4: Run `go test ./internal/store -v`**

Expected: PASS for both implementations.

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/config go.mod go.sum
git commit -m "feat: add S3-compatible transcript storage"
```

### Task 7: Build the HTML Viewer and Local Browser

**Files:**
- Create: `internal/web/server.go`
- Create: `internal/web/handlers.go`
- Test: `internal/web/server_test.go`
- Create: `internal/web/templates/{layout,home,directory,transcript,upload}.html`
- Create: `internal/web/static/app.css`
- Create: `internal/web/static/app.js`
- Modify: `internal/cli/commands.go`

**Interfaces:**
- Consumes: discovery, library service, and store
- Produces: `web.New(ServerConfig) http.Handler`

- [ ] **Step 1: Write failing route and escaping tests**

```go
func TestTranscriptEscapesContentAndShowsRawEvent(t *testing.T) {
    h := newTestServer(t, packageWithText("<script>alert(1)</script>"))
    rr := httptest.NewRecorder()
    h.ServeHTTP(rr, httptest.NewRequest("GET", "/sessions/s_123", nil))
    if rr.Code != http.StatusOK { t.Fatalf("status = %d", rr.Code) }
    if strings.Contains(rr.Body.String(), "<script>alert(1)</script>") { t.Fatal("unescaped transcript") }
    if !strings.Contains(rr.Body.String(), "future_event") { t.Fatal("raw event missing") }
}
func TestHealthz(t *testing.T) {
    rr := httptest.NewRecorder()
    newTestServer(t, fixturePackage()).ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
    if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "ok" { t.Fatalf("%d %q", rr.Code, rr.Body.String()) }
}
```

- [ ] **Step 2: Run `go test ./internal/web -v`**

Expected: FAIL because server and templates do not exist.

- [ ] **Step 3: Implement embedded templates and routes**

Add `/`, `/live`, `/live/{provider}/{sessionID}`, `/library`,
`/users/{slug}`, `/projects/{slug}`, `/sessions/{id}`, `/upload`, and
`/healthz`. The live-session listing uses checkboxes to import one or more
completed sessions into the library, and opening a live-session route parses
without copying. Use `<details>` for tools/raw events, escaped template
rendering, anchored event IDs, prompt navigation, and displayed non-clickable
tags. Resolve live routes by validated provider/session ID against the current
discovery catalog; never turn route text into a path. Send a restrictive
content-security policy, `X-Content-Type-Options: nosniff`, and
`Referrer-Policy: same-origin`; serve static assets with fixed content types.
JavaScript only copies anchors and enhances expansion. Wire `serve --open`.

- [ ] **Step 4: Run `go test ./...`**

Expected: PASS with core pages functional without JavaScript.

- [ ] **Step 5: Commit**

```bash
git add internal/web internal/cli/commands.go
git commit -m "feat: add server-rendered transcript viewer"
```

### Task 8: Authenticate and Authorize Mutations

**Files:**
- Create: `internal/auth/identity.go`
- Create: `internal/auth/proxy.go`
- Create: `internal/auth/oidc.go`
- Create: `internal/auth/csrf.go`
- Test: `internal/auth/auth_test.go`
- Modify: `internal/web/server.go`
- Modify: `internal/web/handlers.go`
- Modify: `internal/web/server_test.go`

**Interfaces:**
- Produces: `auth.Provider.Wrap(http.Handler) http.Handler`
- Produces: `auth.FromContext(context.Context) (auth.Identity, bool)`

- [ ] **Step 1: Write failing identity and authorization tests**

```go
func TestProxyIdentity(t *testing.T) {
    p := NewProxy("X-User", "X-Name", mustCIDRs("192.0.2.0/24"))
    h := p.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        id, ok := FromContext(r.Context()); if !ok { t.Fatal("missing identity") }
        fmt.Fprint(w, id.Key+"|"+id.DisplayName)
    }))
    req := httptest.NewRequest("GET", "/", nil); req.RemoteAddr = "192.0.2.10:1234"
    req.Header.Set("X-User", "ada@example.com"); req.Header.Set("X-Name", "Ada")
    rr := httptest.NewRecorder(); h.ServeHTTP(rr, req)
    if rr.Body.String() != "ada@example.com|Ada" { t.Fatalf("body = %q", rr.Body.String()) }
}
func TestDifferentUserCannotDelete(t *testing.T) {
    rr := performAs(t, server, "grace@example.com", "DELETE", "/api/v1/sessions/s_123", nil)
    if rr.Code != http.StatusForbidden { t.Fatalf("status = %d", rr.Code) }
}
func TestBrowserMutationRejectsMissingCSRF(t *testing.T) {
    rr := performAs(t, server, "ada@example.com", "POST", "/api/v1/sessions/s_123/move", validMoveBody)
    if rr.Code != http.StatusForbidden { t.Fatalf("status = %d", rr.Code) }
}
func TestProxyModeRejectsUntrustedPeer(t *testing.T) {
    req := requestFrom("203.0.113.9:1234"); req.Header.Set("X-User", "ada@example.com")
    if rr := serve(proxyServer, req); rr.Code != http.StatusUnauthorized { t.Fatalf("status = %d", rr.Code) }
}
```

- [ ] **Step 2: Run `go test ./internal/auth ./internal/web -run 'TestProxy|TestDifferent' -v`**

Expected: FAIL because identity and mutation authorization do not exist.

- [ ] **Step 3: Implement local, proxy, and OIDC providers**

Proxy mode requires configured headers and an explicit list of trusted proxy CIDRs; strip/ignore identity headers from every other peer. OIDC uses discovery, authorization-code flow with state, nonce, and PKCE, exact issuer/client-ID checks, and a rotated-key signed-and-encrypted HTTP-only Secure SameSite=Lax cookie with an absolute lifetime. Regenerate the session on login. Add origin validation plus synchronizer-token CSRF protection to HTML mutations. Add a CSRF-protected browser action that mints a signed 15-minute bearer token containing normalized user key, issued-at/expiry, unique ID, and `agent-transcripts-api` audience; verify signature, expiry, and audience on API requests. Bearer-authenticated requests do not use proxy headers or cookies, which makes CLI auth work in both proxy and OIDC deployments without storing server-side token state. Add metadata patch, move, and delete handlers with explicit JSON/form content types and revision preconditions. Load stored metadata and compare `UploaderKey` with the normalized context identity immediately before each mutation; ignore client-supplied uploader fields. Return generic authentication errors and never log callback parameters, claims, cookies, or tokens.

- [ ] **Step 4: Run `go test ./internal/auth ./internal/web -v`**

Expected: PASS, including an OIDC callback test using a local test issuer.

- [ ] **Step 5: Commit**

```bash
git add internal/auth internal/web go.mod go.sum
git commit -m "feat: authenticate hosted transcript access"
```

### Task 9: Publish Through Browser and CLI

**Files:**
- Create: `internal/publish/client.go`
- Test: `internal/publish/client_test.go`
- Modify: `internal/web/handlers.go`
- Modify: `internal/web/server_test.go`
- Modify: `internal/cli/commands.go`

**Interfaces:**
- Produces: `publish.Client.Upload(ctx context.Context, req Request) (Result, error)`
- Consumes: parser registry, authenticated identity, and store

- [ ] **Step 1: Write failing upload and idempotency tests**

```go
func TestUploadReparsesAndAttributesIdentity(t *testing.T) {
    rr := uploadFixtureAs(t, server, "ada@example.com", "claude-session.jsonl", map[string]string{
        "destination": "projects/platform", "title": "Parser design", "tags": "go,rust,go",
    })
    if rr.Code != http.StatusCreated { t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String()) }
    stored := mustGetStored(t, "s_expected")
    if stored.Metadata.UploaderKey != "ada@example.com" { t.Fatalf("uploader = %q", stored.Metadata.UploaderKey) }
    if !slices.Equal(stored.Metadata.Tags, []string{"go", "rust"}) { t.Fatalf("tags = %v", stored.Metadata.Tags) }
}
func TestRepeatedUploadReturnsExistingURL(t *testing.T) {
    first := uploadFixtureAs(t, server, "ada@example.com", "claude-session.jsonl", fields)
    second := uploadFixtureAs(t, server, "ada@example.com", "claude-session.jsonl", fields)
    if first.Header().Get("Location") != second.Header().Get("Location") { t.Fatal("locations differ") }
}
func TestSameSourceCanPublishToTwoDestinations(t *testing.T) {
    user := uploadFixtureAs(t, server, "ada@example.com", "claude-session.jsonl", userFields)
    project := uploadFixtureAs(t, server, "ada@example.com", "claude-session.jsonl", projectFields)
    if user.Header().Get("Location") == project.Header().Get("Location") { t.Fatal("destinations collapsed") }
}
func TestRawUploadWithoutTerminalEvidenceIsRejected(t *testing.T) {
    rr := uploadFixtureAs(t, server, "ada@example.com", "incomplete-claude.jsonl", fields)
    if rr.Code != http.StatusUnprocessableEntity { t.Fatalf("status = %d", rr.Code) }
}
```

- [ ] **Step 2: Run `go test ./internal/publish ./internal/web -run 'TestUpload|TestRepeated' -v`**

Expected: FAIL because hosted upload and client are missing.

- [ ] **Step 3: Implement server-owned normalization and client upload**

Implement `GET /api/v1/directories`, `POST /api/v1/projects`, and
`POST /api/v1/sessions`. Accept multipart `source`, `destination`, `title`,
`description`, and repeated `tag` on session upload. Enforce request size before
calling `ParseMultipartForm` and do not retain transcript parts in memory or
temporary files after the request. Detect, parse, validate, and store on the
server; reject normalized JSON and uploader identity. Raw hosted uploads must
contain parser-verified terminal evidence; only local import may use filesystem
quiet-period facts. Derive the package ID from content plus validated
destination. Return 201 for a new package and 200 with the same Location for a
repeat to that destination; return a distinct Location for another destination.
Validate returned Locations as same-origin relative paths in the CLI client.
Publishing to a new valid project slug creates that project idempotently. Wire
CLI `upload` to upload only an already-imported library package, prompting only
on a terminal, and read the short-lived bearer token from
`AGENT_TRANSCRIPTS_TOKEN` or a no-echo terminal prompt rather than forwarding
proxy headers. Never accept the token as a command-line flag, persist it, or
include it in an error.

- [ ] **Step 4: Run `go test ./internal/publish ./internal/web ./internal/cli -v`**

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/publish internal/web internal/cli/commands.go
git commit -m "feat: publish transcripts to hosted libraries"
```

### Task 10: Prove the End-to-End Vertical Slice

**Files:**
- Create: `internal/integration/roundtrip_test.go`
- Modify: `internal/cli/commands.go`

**Interfaces:**
- Consumes: the real parser, filesystem store, library service, publish client,
  auth middleware, and HTTP server composition
- Produces: one executable acceptance test that crosses process-facing
  boundaries without replacing application services with mocks

- [ ] **Step 1: Write the failing round-trip test**

```go
func TestImportUploadBrowseAndAuthorize(t *testing.T) {
    local, hosted := startRoundTripServers(t)
    imported := importFixture(t, local, "claude-session.jsonl", completedFacts())
    published := uploadAs(t, hosted, imported.ID, "ada@example.com", "projects/platform")
    assertTranscriptContainsEscapedPrompt(t, hosted, published.Location)
    assertRepeatUploadLocation(t, hosted, imported.ID, published.Location)
    assertMutationForbidden(t, hosted, published.Location, "grace@example.com")
}
```

The harness uses temporary filesystem stores and an `httptest` hosted server,
but otherwise uses production composition. Add a Codex subtest and assert that
unknown raw events survive both stored normalized JSON and rendered HTML.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/integration -run TestImportUploadBrowseAndAuthorize -v`

Expected: FAIL until CLI/application composition exposes injectable constructors.

- [ ] **Step 3: Add only the composition seams required by the test**

Move constructor wiring out of flag parsing into a `cli.Dependencies` value;
keep `main` responsible only for OS streams, signals, and exit status. Do not
introduce another service layer or test-only production branches.

- [ ] **Step 4: Run full verification, including races**

```bash
go test -race ./...
go vet ./...
```

Expected: PASS with both provider round trips, idempotency, authorization,
escaping, and raw-event preservation covered.

- [ ] **Step 5: Commit**

```bash
git add internal/integration internal/cli/commands.go
git commit -m "test: prove transcript round trip"
```

### Task 11: Package the Skill, Plugin, Container, and Docs

**Files:**
- Create: `skills/publish-transcript/SKILL.md`
- Create: `.claude-plugin/plugin.json`
- Create: `.claude-plugin/marketplace.json`
- Create: `Dockerfile`
- Create: `config.example.yaml`
- Create: `README.md`

**Interfaces:**
- Consumes: installed `agent-transcripts import` and `upload`
- Produces: Claude marketplace and portable `npx skills add` installations

- [ ] **Step 1: Create the plugin manifests and skill**

The skill verifies the binary exists, rejects the active session, resolves/imports a completed session, collects destination and optional metadata, shows the exact publication summary, invokes `agent-transcripts upload`, and returns the URL. It must not suggest bypassing eligibility with a direct path or raw browser upload. State that installation never authorizes background uploads. Name the plugin `agent-transcripts`; marketplace source is `./`. Validate both JSON manifests with `python -m json.tool` and the repository layout with the Claude plugin CLI documented in the README.

- [ ] **Step 2: Add the minimal container and config examples**

Use a pinned Go toolchain in a multi-stage Dockerfile with `CGO_ENABLED=0`, `-trimpath`, and an explicit target architecture, then copy the static Linux binary into a pinned-by-digest non-root distroless image with `ENTRYPOINT ["/agent-transcripts"]`. Add a writable volume path only for filesystem mode. Include local filesystem, hosted proxy/filesystem, and hosted OIDC/S3 YAML examples, including trusted proxy CIDRs, external origin, cookie-key environment variable, upload limits, and bearer-token configuration. Refer to environment-variable names, never secret values.

- [ ] **Step 3: Document first-run paths**

README covers binary installation, local browsing, import/upload, hosted filesystem and S3 operation, proxy/OIDC setup, TLS-at-the-proxy requirements, trusted proxy network isolation, signing/encryption key rotation, Claude plugin installation, `npx skills` installation, and v1 privacy/completion limitations. Explicitly document that raw browser uploads without terminal evidence are rejected and that logs are metadata-only.

- [ ] **Step 4: Run final verification**

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o ./bin/agent-transcripts ./cmd/agent-transcripts
./bin/agent-transcripts version
docker build -t agent-transcripts:test .
```

Expected: tests, race detector, and vet pass; version prints `agent-transcripts dev`; image builds and `docker inspect` reports a non-root user.

- [ ] **Step 5: Commit**

```bash
git add skills .claude-plugin Dockerfile config.example.yaml README.md
git commit -m "docs: package agent transcripts for first release"
```

## Final Acceptance Check

- [ ] Run `go test ./...` and `go vet ./...` successfully.
- [ ] Build the binary and container.
- [ ] Start local mode and confirm live and library pages render.
- [ ] Import one completed Claude fixture and one completed Codex fixture.
- [ ] Upload both to a hosted filesystem-backed server and open stable URLs.
- [ ] Confirm a second user can browse but cannot mutate them.
- [ ] Confirm repeated upload returns the existing URL.
- [ ] Confirm the same source uploaded to a second destination gets a distinct URL.
- [ ] Confirm an incomplete raw browser upload is rejected and an old local file is accepted only after descriptor revalidation.
- [ ] Confirm unknown events render in expandable raw-details blocks.
- [ ] Run concurrent identical puts against filesystem and fake S3 and confirm exactly one finalized package is visible.
- [ ] Confirm cross-origin and missing-CSRF browser mutations fail, and untrusted peers cannot assert proxy identity headers.
- [ ] Confirm logs contain neither fixture content nor credentials.
- [ ] Run `git status --short` and confirm the worktree is clean.
