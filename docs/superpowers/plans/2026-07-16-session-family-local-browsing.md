# Project-Scoped Session Families Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Let agent-transcripts discover, browse, import, persist, publish, and render complete provider session families scoped to the current Git worktree by default.

**Architecture:** Introduce provider-neutral SessionFamily and persistent ProjectRef types while provider adapters own source formation, completion evidence, and Claude attachment. Snapshot every member before parsing, derive family semantics centrally, and publish ordered multi-source packages atomically. Schema-v1 reads project into a one-main-source v2 family.

**Tech Stack:** Go standard library, existing html/template UI, filesystem and S3 stores, Go tests.

## Global Constraints

- New imports write normalized schema version 2; schema-v1 packages remain readable without rewrite.
- Default local behavior is scoped to the canonical current Git worktree; --all-projects is the cross-project opt-in.
- Persist ProjectRef, never ProjectScope.CanonicalRoot, source paths, or upload filenames.
- A family has at most 256 sources and 64 MiB aggregate uncompressed bytes.
- Selectors are f_<sha256(provider, canonical-main-path)> and only exact rediscovered-map hits are authorized.
- Local sources use descriptor-relative no-follow opens; unsupported platforms return an error.
- Parse only private two-pass-verified snapshots; any member failure stores no package.
- Hosted upload accepts one source and zero or more child parts and requires provider-terminal evidence.
- Preserve escaping, CSP, CSRF, loopback binding, ownership/revision checks, existing URLs, and raw diagnostics.

---

## File Structure

- internal/session/{model,identity,validate}.go: v2 family, project, manifest, package, identity, and validation contracts.
- internal/parser/{parser,codex,claude}.go: provider record preservation, Codex deduplication, Claude identity and attachment.
- internal/discovery/{project,discovery,snapshot}.go: project scope, family formation, safe snapshots, eligibility.
- internal/store/{store,filesystem,s3}.go: versioned reads and atomic multi-source persistence.
- internal/library/service.go, internal/publish/client.go, internal/web/handlers.go: import and multipart transport.
- internal/cli/commands.go: browse, picker, scope-aware import/serve.
- internal/review/review.go and internal/web/templates/transcript.html: nested family review.

### Task 1: Define project and family contracts

**Files:**
- Modify: internal/session/model.go, internal/session/identity.go, internal/session/validate.go, internal/session/model_test.go
- Create: internal/session/family_test.go

**Interfaces:**
- Produces ProjectRef, ProjectScope, SessionFamily, ChildSession, FamilyCompletion, SourceManifest, SourceBlob, and SourceFactEntry.
- Produces ValidateFamily(SessionFamily) error and ContentIDForManifest(string, SourceManifest) string.

- [ ] **Step 1: Write failing family semantics tests**

~~~go
func TestValidateFamilyRejectsNonDerivedBounds(t *testing.T) {
    f := terminalFamily(t, at(5), at(30))
    if err := session.ValidateFamily(f); err != nil { t.Fatal(err) }
    f.EndedAt = at(20)
    if err := session.ValidateFamily(f); err == nil { t.Fatal("accepted mismatched end") }
}
func TestContentIDForManifestIgnoresChildInputOrder(t *testing.T) {
    if session.ContentIDForManifest("claude", manifest("b", "a")) != session.ContentIDForManifest("claude", manifest("a", "b")) {
        t.Fatal("content ID depends on child enumeration")
    }
}
~~~

- [ ] **Step 2: Run focused tests and verify they fail**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/session -run 'TestValidateFamily|TestContentIDForManifest' -count=1

Expected: FAIL because v2 types and functions do not exist.

- [ ] **Step 3: Implement the v2 model and validation**

~~~go
type ProjectRef struct { Kind, Key, DisplayName string }
type ProjectScope struct { Ref ProjectRef; CanonicalRoot string }
type FamilyCompletion struct { Status, Reason string; LastEventAt time.Time }
type ChildSession struct { AgentID, ParentToolCallID, AgentType, Description string; Attached bool; Session Session }
type SessionFamily struct { SchemaVersion int; ID, Provider, ProviderSessionID string; Project ProjectRef; Main Session; Children []ChildSession; StartedAt, EndedAt time.Time; Completion FamilyCompletion }
type SourceEntry struct { Role, AgentID, Checksum, Name string; Bytes int64 }
type SourceManifest struct { SchemaVersion int; Provider, SessionID string; Sources []SourceEntry }
~~~

Require one main, unique child agent IDs, paired attachment fields, matching projects, and recomputed start/end/last-event/completion values. Canonicalize as main first and children by agent ID with source/main.jsonl and source/children/<agent>.jsonl. Extend Package for family/source slices and retain a v1 one-source projection.

- [ ] **Step 4: Run package tests**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/session -count=1

Expected: PASS.

- [ ] **Step 5: Commit**

~~~bash
git add internal/session
git commit -m "feat: add session family model"
~~~

### Task 2: Normalize provider records and attach Claude children

**Files:**
- Modify: internal/parser/parser.go, internal/parser/codex.go, internal/parser/claude.go, internal/parser/parser_test.go
- Create: internal/parser/testdata/claude-family-main.jsonl, internal/parser/testdata/claude-family-child.jsonl

**Interfaces:**
- Produces provider record facts and parser.AttachClaudeChildren(main session.Session, children []parser.Child) ([]session.ChildSession, error).

- [ ] **Step 1: Write failing parser tests**

~~~go
func TestCodexPairsResponseUserWithCanonicalEventUser(t *testing.T) {
    got := parse(t, responseUser("Inspect") + "\n" + eventUser("Inspect"))
    if prompts(got) != []string{"Inspect"} { t.Fatalf("prompts = %#v", prompts(got)) }
    if raws(got) != 1 { t.Fatal("paired evidence was discarded") }
}
func TestClaudeAttachmentUsesAgentIDNotDescription(t *testing.T) {
    got, err := ParseClaudeFamily(mainFixture, []io.Reader{childFixture})
    if err != nil { t.Fatal(err) }
    if !got.Children[0].Attached || got.Children[0].ParentToolCallID != "tool-9" { t.Fatalf("%#v", got.Children[0]) }
}
~~~

- [ ] **Step 2: Run focused tests and verify they fail**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/parser -run 'TestCodexPairs|TestClaudeAttachment' -count=1

Expected: FAIL because both Codex user records are visible and no family attachment exists.

- [ ] **Step 3: Implement provider-local rules**

Create records and boundaries before visibility. Canonical event_msg/user_message is visible; same-boundary preceding equal response user is raw paired evidence; recognized environment/skill response envelopes are raw; unpaired response users stay visible.

For Claude, require root filename and record session-ID consistency; parse agent IDs, parent chains, CWDs, block/message IDs, compaction, summaries, tool results, and terminal states. Merge compatible assistant snapshots by message/block IDs, reject conflicting repeated blocks, and attach only with agentId -> toolUseResult.agentId -> tool_result.tool_use_id -> Agent|Task. Missing chain is unattached; ambiguous identity is invalid.

- [ ] **Step 4: Run parser verification**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/parser -count=1

Expected: PASS for response-only logs, boundary-distinct duplicates, Agent/Task attachment, duplicate descriptions, compact summaries, and conflicting identities.

- [ ] **Step 5: Commit**

~~~bash
git add internal/parser
git commit -m "feat: normalize provider session families"
~~~

### Task 3: Resolve project scope, form families, and snapshot safely

**Files:**
- Create: internal/discovery/project.go, internal/discovery/snapshot.go
- Modify: internal/discovery/discovery.go, internal/discovery/discovery_test.go

**Interfaces:**
- Produces ResolveProjectScope(cwd string) (session.ProjectScope, error), DiscoverFamilies(context.Context, Roots, session.ProjectScope, DiscoverOptions) ([]SessionFamilyCandidate, error), and SnapshotFamily(SessionFamilyCandidate) (SnapshotFamily, error).

- [ ] **Step 1: Write failing scope and formation tests**

~~~go
func TestDiscoverFamiliesKeepsClaudeChildrenOffTheTopLevel(t *testing.T) {
    got, err := DiscoverFamilies(context.Background(), roots, scope, opts)
    if err != nil { t.Fatal(err) }
    if len(got) != 1 || len(got[0].Children) != 1 { t.Fatalf("%#v", got) }
}
func TestDiscoverFamiliesFiltersToCanonicalWorktree(t *testing.T) {
    got, _ := DiscoverFamilies(context.Background(), roots, scopeFor(t, repoA), opts)
    if len(got) != 1 || got[0].Project.Key != scopeFor(t, repoA).Ref.Key { t.Fatalf("scope leak: %#v", got) }
}
~~~

- [ ] **Step 2: Run focused discovery tests and verify they fail**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/discovery -run TestDiscoverFamilies -count=1

Expected: FAIL because discovery returns flat candidates with no scope.

- [ ] **Step 3: Implement scope, formation, and safe snapshots**

Resolve symlinks, use git -C <cwd> rev-parse --show-toplevel, and hash kind plus canonical root; use labelled canonical-directory fallback outside Git. Form Claude only from root <session>.jsonl plus exact <session>/subagents/agent-*.jsonl; reject orphans/mixed IDs and cross-project children. Codex stays one-source. Filter default discovery by project and derive opaque main-path keys.

On Unix, traverse configured-root-relative components with directory FDs, openat, O_NOFOLLOW, and fstat; add equivalent Windows reparse-point checks and reject unsupported platforms. Copy all members into a private 0700 directory with 0600 files under one aggregate limiter; hash each descriptor twice around identity/change-time checks; parse snapshots only. Reject changing, invalid, incomplete, oversized, or over-count families.

- [ ] **Step 4: Run discovery verification**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/discovery -count=1

Expected: PASS for root/nested/symlink launches, linked worktrees, directory fallback, filtering, child formation, stale keys, aggregate limits, and replacement attacks.

- [ ] **Step 5: Commit**

~~~bash
git add internal/discovery
git commit -m "feat: discover scoped session families"
~~~

### Task 4: Persist atomic family packages with v1-compatible reads

**Files:**
- Modify: internal/store/store.go, internal/store/filesystem.go, internal/store/s3.go, internal/store/store_test.go, internal/store/s3_test.go

**Interfaces:**
- Produces Store.PutFamily(context.Context, session.Package) (bool, error); retains PutSession as a one-main adapter.

- [ ] **Step 1: Write failing store tests**

~~~go
func TestFilesystemPutFamilyReadsEveryMember(t *testing.T) {
    st, pkg := NewFilesystem(t.TempDir()), familyPackage(t, "main", "child")
    created, err := st.PutFamily(context.Background(), pkg)
    if err != nil || !created { t.Fatalf("%v, %v", created, err) }
    got, err := st.GetSession(context.Background(), pkg.ID)
    if err != nil || len(got.Sources) != 2 { t.Fatalf("%#v, %v", got, err) }
}
func TestLegacyPackageReadsAsOneSourceFamily(t *testing.T) {
    st := NewFilesystem(t.TempDir())
    writeLegacyPackage(t, st, legacyPackage(t))
    got, err := st.GetSession(context.Background(), legacyPackage(t).ID)
    if err != nil || got.Family.Main.ID == "" || len(got.Family.Children) != 0 || len(got.Sources) != 1 { t.Fatalf("%#v, %v", got, err) }
}
~~~

- [ ] **Step 2: Run focused store tests and verify they fail**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/store -run 'TestFilesystemPutFamily|TestLegacyPackage' -count=1

Expected: FAIL because stores have singular source/session fields.

- [ ] **Step 3: Implement atomic versioned storage**

Validate manifest hashes and derived IDs. Filesystem writes ordered blobs, normalized family JSON, facts, metadata, and manifest to a temporary package directory; fsyncs; renames; then publishes final package manifest. S3 writes immutable objects then final manifest. Missing final manifests are nonexistent. Decode v1 wire data to a one-main v2 family without rewriting.

- [ ] **Step 4: Run store tests**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/store -count=1

Expected: PASS for deterministic IDs, idempotency, partial invisibility, changed children, and v1 projection.

- [ ] **Step 5: Commit**

~~~bash
git add internal/store internal/session
git commit -m "feat: persist session families atomically"
~~~

### Task 5: Import and upload one complete family transaction

**Files:**
- Modify: internal/library/service.go, internal/library/service_test.go, internal/publish/client.go, internal/publish/client_test.go, internal/web/handlers.go, internal/web/server_test.go

**Interfaces:**
- Produces library.Service.ImportFamilyWithStatus(context.Context, discovery.SnapshotFamily, ImportAttrs) (session.Metadata, bool, error).

- [ ] **Step 1: Write failing service and hosted multipart tests**

~~~go
func TestImportFamilyPersistsMainAndChildTogether(t *testing.T) {
    md, created, err := library.New(st, library.AllowLocalQuietEvidence()).ImportFamilyWithStatus(ctx, snapshotFamily(t), attrs)
    if err != nil || !created { t.Fatalf("%#v %v %v", md, created, err) }
    got, _ := st.GetSession(ctx, md.ID)
    if len(got.Family.Children) != 1 || len(got.Sources) != 2 { t.Fatalf("%#v", got) }
}
func TestUploadRejectsUnattachedNonterminalChild(t *testing.T) {
    rr := postFamilyMultipart(t, newTestServer(t), terminalMainFixture, incompleteChildFixture)
    if rr.Code != http.StatusUnprocessableEntity { t.Fatalf("status = %d", rr.Code) }
    if countPackages(t) != 0 { t.Fatal("stored incomplete family") }
}
~~~

- [ ] **Step 2: Run focused tests and verify they fail**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/library ./internal/publish ./internal/web -run 'TestImportFamily|TestUploadRejectsUnattached' -count=1

Expected: FAIL because all transport is one source.

- [ ] **Step 3: Implement import and transport**

Import verified snapshots, derive all identity/project/attachment/completion/manifest fields server-side, and call PutFamily once. Keep one-source ImportWithStatus adapter. Hosted requests require a terminal main; completed parent Agent/Task may prove attached child terminal state; unattached nonterminal children fail. Accept only known fields and source/child parts, set aggregate limits before parsing, trust no filename/agent/checksum/project input, and send package main then sorted children from the client.

- [ ] **Step 4: Run transport tests**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/library ./internal/publish ./internal/web -count=1

Expected: PASS, including existing one-source upload and zero persistence after a bad child.

- [ ] **Step 5: Commit**

~~~bash
git add internal/library internal/publish internal/web
git commit -m "feat: import and upload session families"
~~~

### Task 6: Make focused browse the default command

**Files:**
- Modify: internal/cli/commands.go, internal/cli/commands_test.go, internal/web/server.go, internal/web/handlers.go, internal/web/server_test.go, README.md

**Interfaces:**
- Adds browse [--family key|--latest|--no-open|path] [--all-projects].
- Extends serve and import with --all-projects and project-first selection.

- [ ] **Step 1: Write failing CLI and focused-server tests**

~~~go
func TestRunWithoutSubcommandBrowses(t *testing.T) {
    calls := 0
    code := runWithBrowse(t, nil, func(context.Context, browseOptions) int { calls++; return 0 })
    if code != 0 || calls != 1 { t.Fatalf("code=%d calls=%d", code, calls) }
}
func TestFocusedServerRejectsCatalogAndOtherFamilies(t *testing.T) {
    h := web.New(web.ServerConfig{FocusedFamily: selected})
    assertStatus(t, h, "/live", http.StatusNotFound)
    assertStatus(t, h, "/live/claude/other", http.StatusNotFound)
}
~~~

- [ ] **Step 2: Run focused tests and verify they fail**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/cli ./internal/web -run 'TestRunWithoutSubcommand|TestFocusedServer' -count=1

Expected: FAIL because no args prints usage and the server exposes catalog routes.

- [ ] **Step 3: Implement project-first flows**

Alias no command to browse. Pick one eligible family newest-first. --family matches only rediscovered in-scope keys; explicit main path discovers native children; --latest chooses newest. Browse binds loopback and exposes selected transcript/static assets only, opening unless --no-open. Persistent serve remains catalog behavior but scopes local discovery by default; project index/routes exist only under --all-projects. Interactive all-project import selects project then families.

- [ ] **Step 4: Run CLI and web tests**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/cli ./internal/web -count=1

Expected: PASS for stale/tampered selectors, noninteractive behavior, all-project opt-in, browse isolation, loopback, and existing security routes.

- [ ] **Step 5: Commit**

~~~bash
git add internal/cli internal/web README.md
git commit -m "feat: browse scoped session families"
~~~

### Task 7: Render nested delegated work and verify end-to-end compatibility

**Files:**
- Modify: internal/review/review.go, internal/review/review_test.go, internal/web/handlers.go, internal/web/templates/transcript.html, internal/web/static/app.css, internal/integration/roundtrip_test.go

**Interfaces:**
- Produces review.ProjectFamily(session.SessionFamily) review.FamilyTranscript with main turns, attached child map, and unattached children.

- [ ] **Step 1: Write failing review and HTML tests**

~~~go
func TestProjectFamilyAttachesChildAtParentToolCall(t *testing.T) {
    got := review.ProjectFamily(familyWithAttachedChild())
    if len(got.Attached["call-1"]) != 1 || len(got.Unattached) != 0 { t.Fatalf("%#v", got) }
}
func TestTranscriptRendersUnattachedDelegatedWorkInDetails(t *testing.T) {
    if body := renderFamily(t, familyWithUnattachedChild()); !strings.Contains(body, "Unattached delegated work") || !strings.Contains(body, "<details") {
        t.Fatalf("%s", body)
    }
}
~~~

- [ ] **Step 2: Run focused tests and verify they fail**

Run: GOCACHE="$PWD/.go-cache" go test ./internal/review ./internal/web -run 'TestProjectFamily|TestTranscriptRendersUnattached' -count=1

Expected: FAIL because review and templates only support one flat session.

- [ ] **Step 3: Implement server-rendered family review**

Index main prompts only. Attach child details below its parent Agent/Task event with stable anchors; show each child prompt index, stream, completion, type, description, and diagnostics. Render unattached children in collapsed Unattached delegated work, ordered start time then agent ID. Escape all provider material and retain raw evidence.

- [ ] **Step 4: Run full verification**

Run: GOCACHE="$PWD/.go-cache" go test ./... -count=1 && go vet ./... && git diff --check

Expected: all tests pass, vet exits zero, and diff check emits nothing.

- [ ] **Step 5: Commit**

~~~bash
git add internal/review internal/web internal/integration
git commit -m "feat: render delegated session work"
~~~

## Plan Self-Review

- Spec coverage: Tasks 1–2 cover model and provider contracts; Task 3 covers scope, discovery, eligibility, and safe snapshots; Tasks 4–5 cover atomic storage and multipart transport; Task 6 covers command/navigation contracts; Task 7 covers review rendering, compatibility, and end-to-end verification.
- Placeholder scan: every task states its files, interfaces, test gate, implementation boundary, and commit.
- Type consistency: later tasks consume SessionFamily, SourceManifest, SessionFamilyCandidate, and SnapshotFamily established by Tasks 1–3; legacy Session remains per-source and old APIs become adapters.
