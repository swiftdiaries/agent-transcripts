# V2 Session Family Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make v2 session families release-ready by grouping Codex parent/subagent rollouts correctly, restoring retry-safe S3 persistence, enforcing terminal-family upload policy, snapshotting local sources without path races, and selecting live families only by opaque family key.

**Architecture:** Extend the provider-neutral family model with a direct parent-session relationship while keeping schema-v2 reads backward compatible. Codex adapters derive a validated parent graph from `session_meta`; Claude keeps its provider-identity attachment chain. Discovery parses safely opened descriptors only to establish eligibility and scope; every preview/import parse consumes a private two-pass-verified snapshot. Hosted completion is derived once in the library boundary, and every live selection rediscovery uses the opaque family key.

**Tech Stack:** Go 1.23, Go standard library, `golang.org/x/sys`, existing `html/template` UI, filesystem and S3 stores, Go tests.

## Global Constraints

- New imports continue to write normalized schema version 2; schema-v1 and existing schema-v2 packages remain readable without rewrite.
- Default local behavior remains scoped to the canonical current Git worktree; `--all-projects` remains the only cross-project opt-in.
- Persist `ProjectRef`, never `ProjectScope.CanonicalRoot`, configured source roots, absolute source paths, or multipart filenames.
- A family has at most 256 sources and 64 MiB aggregate uncompressed bytes.
- Codex relationships come only from `session_meta.payload.id`, `parent_thread_id`, `thread_source`, and structured `source.subagent` evidence; timestamps and prompt text never establish parentage.
- A legacy Codex rollout with both `thread_source` and `parent_thread_id` absent remains a one-source root. An explicit `thread_source=user` rollout is a root only when `parent_thread_id` is absent. Every explicit `thread_source=subagent` rollout requires a non-empty matching parent plus exactly one recognized `source.subagent` shape.
- Orphaned, duplicated, malformed, or cyclic Codex components are excluded as components and never become top-level families; they do not make unrelated valid root families undiscoverable.
- Selectors remain `f_<sha256(provider, canonical-main-path)>`; only an exact hit in the freshly rediscovered authorized map may render or import a family.
- Local sources use configured-root-relative no-follow opens and two-pass hash verification; unsupported platforms return `safe source open unsupported`.
- Preview, import, and persistence parse only private snapshots; discovery may parse its safely opened descriptor to establish eligibility and project scope. Any member failure closes and removes the entire snapshot and stores no package.
- Hosted upload accepts only `provider_terminal` families. A recognized terminal parent Agent/Task result may supply terminal evidence for an attached Claude child; unknown, missing, or ambiguous results do not.
- Preserve escaping, CSP, CSRF, loopback binding, ownership and revision checks, existing stored-session URLs, and raw diagnostics.
- Each behavior-changing task writes and observes its regression before production code, ends with focused verification, and is committed independently. Task 8 is a post-implementation release proof and is expected to pass on its first focused run.

---

## Execution Preconditions

- Use `superpowers:using-git-worktrees` before Task 1. Do not implement on `main` without the user's explicit consent.
- Run `GOCACHE="$PWD/.go-cache" go test ./... -count=1 -timeout=180s` in the selected workspace. Stop and report any baseline failure before attributing it to this plan.
- The shared `.superpowers/sdd/progress.md` may describe an older plan. Treat entries as resumable only when its header is exactly `Plan: docs/superpowers/plans/2026-07-17-v2-session-family-remediation.md`. If the header differs or is absent, preserve the old ledger under `.superpowers/sdd/archive/` and initialize a fresh ledger with that exact header before dispatching Task 1.
- Ensure this reviewed remediation plan is tracked on the execution branch before Task 1. An untracked file in the main checkout will not appear in a new worktree, so do not defer tracking it until the release task.

---

## File Structure

- `docs/superpowers/specs/2026-07-16-session-family-local-browsing-design.md`: correct the Codex one-source assumption and record next-wave invariants.
- `internal/session/model.go`, `internal/session/validate.go`: backward-compatible parent-session fields, normalized parent-result status, and graph validation.
- `internal/parser/codex.go`, `internal/parser/parser.go`: preserve Codex session origin and construct provider-proven child entries.
- `internal/parser/testdata/codex-family-*.jsonl`: root, worker, and guardian provenance fixtures.
- `internal/discovery/family.go`: form Codex families from the parent graph and exclude invalid components.
- `internal/discovery/safeopen_unix.go`, `safeopen_linux.go`, `safeopen_darwin.go`, `safeopen_windows.go`, `safeopen_unsupported.go`: platform-specific descriptor-safe opening and identity extraction.
- `internal/discovery/snapshot.go`: private family snapshots, aggregate limits, two-pass verification, and cleanup.
- `internal/library/service.go`: provider-neutral family construction and one authoritative completion derivation.
- `internal/store/s3.go`: retry ownership recovery for family reclaim claims.
- `internal/review/review.go`, `internal/web/handlers.go`, `internal/web/templates/transcript.html`: nested Codex descendants and Claude tool-call attachments.
- `internal/web/templates/directory.html`, `internal/web/handlers.go`: opaque family-key links and import selections.
- `internal/integration/roundtrip_test.go`: end-to-end Codex family import, persistence, reread, and rendering.

### Task 1: Restore Retry-Safe S3 Family Persistence

**Files:**
- Modify: `internal/store/s3.go:217-288,976-1009`
- Modify: `internal/store/s3_test.go:452-471`

**Interfaces:**
- Consumes: `S3.reclaimKey`, `S3.identicalFamily`, and conditional `S3API.PutObject` behavior.
- Produces: `claimFamilyReclaim(context.Context, string, []byte) (familyClaimState, error)` where `familyClaimState` is `familyClaimAbsent`, `familyClaimOwned`, or `familyClaimWaiting`.

- [ ] **Step 1: Tighten the response-loss regression so it fails quickly**

Add a deadline to the retry call and require successful convergence:

```go
func TestS3ReclaimClaimResponseLossRetryConverges(t *testing.T) {
    fake := newFakeS3()
    st := NewS3(fake, "bucket", "prod")
    p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
    if _, err := st.PutSession(context.Background(), p); err != nil { t.Fatal(err) }
    if err := st.DeleteSession(context.Background(), p.ID, "ada", currentRevision(t, st, p.ID)); err != nil { t.Fatal(err) }

    reput := p
    reput.Metadata.Title = "retry"
    fake.failAfterPut("prod/users/ada/"+p.ID+"/.reclaim", errors.New("response lost"))
    if _, err := st.PutSession(context.Background(), reput); err == nil { t.Fatal("wanted response loss") }

    ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
    defer cancel()
    created, err := st.PutSession(ctx, reput)
    if err != nil || !created { t.Fatalf("retry = %v,%v", created, err) }
}
```

- [ ] **Step 2: Run the focused regression and confirm the existing wait loop fails**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/store -run '^TestS3ReclaimClaimResponseLossRetryConverges$' -count=1`

Expected: FAIL with `context deadline exceeded` from `reconcileFamilyReclaim`.

- [ ] **Step 3: Separate claim acquisition from waiting and recover ownership after response loss**

Add the state type and helper, then use it before publishing the manifest:

```go
type familyClaimState uint8

const (
    familyClaimAbsent familyClaimState = iota
    familyClaimOwned
    familyClaimWaiting
)

func (s *S3) claimFamilyReclaim(ctx context.Context, key string, metadata []byte) (familyClaimState, error) {
    if _, err := s.client.PutObject(ctx, s.bucket, key, metadata, S3Condition{IfNoneMatch: true}); err == nil {
        return familyClaimOwned, nil
    } else if !errors.Is(err, ErrS3PreconditionFailed) {
        return familyClaimAbsent, err
    }
    claim, err := s.get(ctx, key, "metadata.json")
    if errors.Is(err, ErrS3NotFound) { return familyClaimAbsent, nil }
    if err != nil { return familyClaimAbsent, err }
    if checksum(claim.Body) != checksum(metadata) { return familyClaimWaiting, nil }
    // A byte-identical orphan claim is this idempotent operation's durable
    // ownership token after a lost response. The retry must resume publication.
    return familyClaimOwned, nil
}
```

In `PutFamily`, replace the boolean assignment around the reclaim `PutObject` with a small acquisition loop. Retry `familyClaimAbsent`, treat `familyClaimOwned` as permission to resume metadata and manifest publication, and call `reconcileFamilyReclaim` only for `familyClaimWaiting`. If reconciliation reports that the competing manifest settled, return its identical/conflict result; if the claim disappeared, retry acquisition rather than publishing without a claim. Delete an owned byte-identical claim after this call either publishes the manifest or observes an identical winning manifest. Keep claim deletion best-effort so the existing delete-response-loss idempotency test remains valid.

- [ ] **Step 4: Run all store tests**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/store -count=1 -timeout=90s`

Expected: PASS, including response-loss, competing claimant, cancellation, delete, move, schema-v1 adapter, and schema-v2 family tests.

- [ ] **Step 5: Commit the persistence repair**

```bash
git add internal/store/s3.go internal/store/s3_test.go
git commit -m "fix: recover S3 family reclaim retries"
```

### Task 2: Add Provider-Neutral Parent-Session Relationships

**Files:**
- Modify: `docs/superpowers/specs/2026-07-16-session-family-local-browsing-design.md:180-250,287-310,408-420`
- Modify: `internal/session/model.go:51-88`
- Modify: `internal/session/validate.go:94-155`
- Modify: `internal/session/model_test.go`

**Interfaces:**
- Consumes: existing `Session`, `ChildSession`, and `SessionFamily` schema-v2 JSON.
- Produces: `SessionOrigin`, `Session.Origin`, `ChildSession.ParentSessionID`, and `familyMemberByID(SessionFamily) (map[string]Session, error)`.

- [ ] **Step 1: Replace the obsolete Codex one-source rule in the design**

Write the following contract into the Codex formation and completion sections:

```markdown
For Codex, a rollout whose `session_meta.payload.thread_source` is `user` and
has no `parent_thread_id` is a family root. For compatibility with existing
Codex logs, a rollout with both fields absent remains an ungrouped one-source
root. A rollout with
`thread_source=subagent` is a child only when its non-empty
`parent_thread_id` resolves to the root or another descendant in the same
project. `source.subagent.thread_spawn` supplies agent path, nickname, and
role; `source.subagent.other=guardian` supplies the guardian type. Duplicate
IDs, cycles, cross-project edges, and conflicting parent evidence invalidate
only that connected component. Orphans and malformed explicit subagents are
excluded and never promoted to roots; unrelated valid roots remain visible.
```

Also specify that optional relationship fields are a backward-compatible schema-v2 extension and missing fields in previously stored v2 packages mean a direct child of the main session.

- [ ] **Step 2: Write failing model tests for reachable parents and cycles**

```go
func TestValidateFamilyRejectsUnknownParentSession(t *testing.T) {
    f := terminalFamily(t, at(5), at(30))
    f.Children[0].ParentSessionID = "missing"
    if err := ValidateFamily(f); err == nil { t.Fatal("accepted unknown parent") }
}

func TestValidateFamilyRejectsParentCycle(t *testing.T) {
    f := nestedTerminalFamily(t)
    f.Children[0].ParentSessionID = f.Children[1].Session.ID
    f.Children[1].ParentSessionID = f.Children[0].Session.ID
    if err := ValidateFamily(f); err == nil { t.Fatal("accepted parent cycle") }
}

func TestValidateFamilyReadsLegacyV2DirectChild(t *testing.T) {
    f := terminalFamily(t, at(5), at(30))
    f.Children[0].ParentSessionID = ""
    if err := ValidateFamily(f); err != nil { t.Fatal(err) }
}

func TestValidateFamilyAllowsCodexChildSessionIdentity(t *testing.T) {
    f := nestedCodexTerminalFamily(t)
    if err := ValidateFamily(f); err != nil { t.Fatal(err) }
}

func TestValidateFamilyRejectsCodexChildThatReusesRootID(t *testing.T) {
    f := nestedCodexTerminalFamily(t)
    f.Children[0].Session.ID = f.Main.ID
    f.Children[0].Session.ProviderSessionID = f.Main.ID
    if err := ValidateFamily(f); err == nil { t.Fatal("accepted reused root ID") }
}

func TestValidateRejectsMalformedCodexOrigin(t *testing.T) {
    f := nestedCodexTerminalFamily(t)
    f.Children[0].Session.Origin.Kind = "subagent"
    if err := ValidateFamily(f); err == nil { t.Fatal("accepted unrecognized origin") }
}
```

Add the fixture builders in `model_test.go` beside these tests. `terminalFamily` is the existing valid Claude shape with one child; `nestedTerminalFamily` adds two distinct child sessions; `nestedCodexTerminalFamily` uses root ID `codex-root`, child IDs/agent IDs `codex-worker` and `codex-guardian`, and the guardian parent `codex-worker`. Every builder must derive family timestamps/completion from its members rather than bypassing validation.

- [ ] **Step 3: Run model tests and verify the graph checks are absent**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/session -run 'TestValidateFamilyRejectsUnknownParentSession|TestValidateFamilyRejectsParentCycle|TestValidateFamilyReadsLegacyV2DirectChild' -count=1`

Expected: FAIL because unknown parents and cycles are accepted.

- [ ] **Step 4: Add backward-compatible origin and parent fields**

```go
type SessionOrigin struct {
    Kind            string `json:"kind,omitempty"`
    ParentSessionID string `json:"parent_session_id,omitempty"`
    AgentPath       string `json:"agent_path,omitempty"`
    AgentName       string `json:"agent_name,omitempty"`
    AgentRole       string `json:"agent_role,omitempty"`
}

type Session struct {
    // Add this field to the existing Session definition.
    Origin SessionOrigin `json:"origin,omitempty"`
}

type ChildSession struct {
    AgentID          string  `json:"agent_id"`
    ParentSessionID  string  `json:"parent_session_id,omitempty"`
    ParentToolCallID string  `json:"parent_tool_call_id,omitempty"`
    AgentType        string  `json:"agent_type,omitempty"`
    Description      string  `json:"description,omitempty"`
    Attached         bool    `json:"attached"`
    Session          Session `json:"session"`
}
```

Use root identity plus distinct Codex child session IDs as graph nodes:

```go
func familyMemberByID(f SessionFamily) (map[string]Session, error) {
    members := map[string]Session{f.Main.ID: f.Main}
    if f.Provider != "codex" { return members, nil }
    for _, child := range f.Children {
        // Codex descendants are graph nodes and must have distinct identities.
        if child.Session.ID == f.Main.ID { return nil, fmt.Errorf("Codex child reuses root session ID") }
        if _, exists := members[child.Session.ID]; exists { return nil, fmt.Errorf("duplicate member session ID") }
        members[child.Session.ID] = child.Session
    }
    return members, nil
}

func validateFamilyParents(f SessionFamily, members map[string]Session) error {
    parentOf := make(map[string]string, len(f.Children))
    for _, child := range f.Children {
        parent := child.ParentSessionID
        if parent == "" { parent = f.Main.ID }
        if _, exists := members[parent]; !exists { return fmt.Errorf("unknown parent session %q", parent) }
        if f.Provider == "claude" {
            if parent != f.Main.ID { return errors.New("Claude child parent is not the family root") }
            continue
        }
        if child.AgentID != child.Session.ID || child.Session.ProviderSessionID != child.Session.ID {
            return errors.New("Codex child identity mismatch")
        }
        if child.Session.Origin.ParentSessionID != parent || child.AgentType != child.Session.Origin.Kind {
            return errors.New("Codex child origin mismatch")
        }
        parentOf[child.Session.ID] = parent
    }
    for child := range parentOf {
        seen := map[string]bool{}
        for current := child; current != f.Main.ID; current = parentOf[current] {
            if current == "" || seen[current] { return fmt.Errorf("family parent graph is cyclic or unreachable") }
            seen[current] = true
        }
    }
    return nil
}
```

For Claude, retain the rule that every child `ProviderSessionID` equals the family root ID and that Claude children cannot themselves be graph parents. For Codex, require every descendant `ProviderSessionID` and `AgentID` to equal its own distinct `Session.ID`, and require the duplicated origin/parent fields to agree. Treat an empty `ParentSessionID` as `f.Main.ID` without rewriting stored JSON; every consumer, including review projection in Task 6, must use the same normalization. Validate `SessionOrigin`: roots have the zero value, child kinds are exactly `thread_spawn` or `guardian`, parent IDs use the existing logical-ID validator, and agent path/name/role strings are valid UTF-8 and bounded by `MaxDescriptionBytes`. Require an attached `ParentToolCallID` to identify exactly one Agent/Task call in the referenced parent session; duplicate or absent matching event IDs are invalid.

- [ ] **Step 5: Run session tests**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/session -count=1`

Expected: PASS for old schema-v2 direct children and new nested graphs.

- [ ] **Step 6: Commit the relationship contract**

```bash
git add docs/superpowers/specs/2026-07-16-session-family-local-browsing-design.md internal/session/model.go internal/session/validate.go internal/session/model_test.go
git commit -m "feat: model nested session family relationships"
```

### Task 3: Parse Codex Origin and Form Parent-Graph Families

**Files:**
- Create: `internal/parser/testdata/codex-family-main.jsonl`
- Create: `internal/parser/testdata/codex-family-worker.jsonl`
- Create: `internal/parser/testdata/codex-family-guardian.jsonl`
- Modify: `internal/parser/codex.go:22-72`
- Modify: `internal/parser/parser_test.go`
- Modify: `internal/discovery/discovery.go:27-40,118-157`
- Modify: `internal/discovery/family.go:14-67`
- Modify: `internal/discovery/discovery_test.go`

**Interfaces:**
- Consumes: `session.SessionOrigin` from Task 2.
- Produces: `Candidate.Origin session.SessionOrigin`, `ChildSourceCandidate.ParentSessionID`, and Codex-aware `FormFamilies([]Candidate, session.ProjectScope) ([]SessionFamilyCandidate, error)`.

- [ ] **Step 1: Add minimal root, worker, and guardian fixtures**

Use unique session IDs and direct parent evidence. Create these complete fixtures so later parser, discovery, and integration tests share one source of truth:

```json
{"timestamp":"2026-07-17T08:00:00Z","type":"session_meta","payload":{"id":"codex-root","cwd":"/repo","thread_source":"user","source":"cli"}}
{"timestamp":"2026-07-17T08:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"Root prompt"}}
{"timestamp":"2026-07-17T08:00:02Z","type":"event_msg","payload":{"type":"task_complete"}}
```

```json
{"timestamp":"2026-07-17T08:01:00Z","type":"session_meta","payload":{"id":"codex-worker","parent_thread_id":"codex-root","cwd":"/repo","thread_source":"subagent","source":{"subagent":{"thread_spawn":{"parent_thread_id":"codex-root","depth":1,"agent_path":"/root/reviewer","agent_nickname":"Ada","agent_role":"reviewer"}}}}}
{"timestamp":"2026-07-17T08:01:01Z","type":"event_msg","payload":{"type":"user_message","message":"Worker prompt"}}
{"timestamp":"2026-07-17T08:01:02Z","type":"event_msg","payload":{"type":"task_complete"}}
```

```json
{"timestamp":"2026-07-17T08:02:00Z","type":"session_meta","payload":{"id":"codex-guardian","parent_thread_id":"codex-worker","cwd":"/repo","thread_source":"subagent","source":{"subagent":{"other":"guardian"}}}}
{"timestamp":"2026-07-17T08:02:01Z","type":"event_msg","payload":{"type":"user_message","message":"Guardian review"}}
{"timestamp":"2026-07-17T08:02:02Z","type":"event_msg","payload":{"type":"task_complete"}}
```

- [ ] **Step 2: Write failing parser provenance tests**

```go
func parseFixture(t *testing.T, name string) session.Session {
    t.Helper()
    f, err := os.Open(filepath.Join("testdata", name))
    if err != nil { t.Fatal(err) }
    defer f.Close()
    got, err := DefaultRegistry().DetectAndParse(context.Background(), f)
    if err != nil { t.Fatal(err) }
    return got
}

func TestCodexPreservesThreadSpawnOrigin(t *testing.T) {
    got := parseFixture(t, "codex-family-worker.jsonl")
    if got.Origin.Kind != "thread_spawn" || got.Origin.ParentSessionID != "codex-root" || got.Origin.AgentPath != "/root/reviewer" || got.Origin.AgentName != "Ada" || got.Origin.AgentRole != "reviewer" {
        t.Fatalf("origin = %#v", got.Origin)
    }
}

func TestCodexPreservesGuardianOrigin(t *testing.T) {
    got := parseFixture(t, "codex-family-guardian.jsonl")
    if got.Origin.Kind != "guardian" || got.Origin.ParentSessionID != "codex-worker" { t.Fatalf("origin = %#v", got.Origin) }
}

func TestCodexAcceptsLegacyRootWithoutThreadSource(t *testing.T) {
    got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(
        `{"type":"session_meta","payload":{"id":"legacy-root"}}`))
    if err != nil || got.Origin != (session.SessionOrigin{}) { t.Fatalf("session=%#v err=%v", got, err) }
}

func TestCodexRejectsMalformedExplicitSubagentOrigin(t *testing.T) {
    for _, input := range []string{
        `{"type":"session_meta","payload":{"id":"missing-parent","thread_source":"subagent","source":{"subagent":{"other":"guardian"}}}}`,
        `{"type":"session_meta","payload":{"id":"unknown-kind","parent_thread_id":"root","thread_source":"subagent","source":{"subagent":{"other":"future"}}}}`,
    } {
        if _, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input)); err == nil {
            t.Fatalf("accepted %s", input)
        }
    }
}
```

- [ ] **Step 3: Run parser tests and verify origin is empty**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/parser -run 'TestCodexPreservesThreadSpawnOrigin|TestCodexPreservesGuardianOrigin' -count=1`

Expected: FAIL with an empty `Session.Origin`.

- [ ] **Step 4: Decode only stable Codex relationship fields**

Extend `codexPayload` and map `session_meta`:

```go
type codexSubagentSource struct {
    ThreadSpawn *struct {
        ParentThreadID string `json:"parent_thread_id"`
        AgentPath      string `json:"agent_path"`
        AgentNickname  string `json:"agent_nickname"`
        AgentRole      string `json:"agent_role"`
    } `json:"thread_spawn,omitempty"`
    Other string `json:"other,omitempty"`
}

// Add to codexPayload:
ParentThreadID string `json:"parent_thread_id"`
ThreadSource   string `json:"thread_source"`
Source         json.RawMessage `json:"source"`
```

Decode `Source` only when it is an object so the ordinary `"source":"cli"` root form remains valid:

```go
func codexOrigin(p codexPayload) (session.SessionOrigin, error) {
    switch p.ThreadSource {
    case "": // Legacy one-source Codex root.
        if p.ParentThreadID != "" { return session.SessionOrigin{}, errors.New("Codex legacy root has parent evidence") }
        return session.SessionOrigin{}, nil
    case "user":
        if p.ParentThreadID != "" { return session.SessionOrigin{}, errors.New("Codex user root has parent evidence") }
        return session.SessionOrigin{}, nil
    case "subagent":
        if p.ParentThreadID == "" { return session.SessionOrigin{}, errors.New("Codex subagent has no parent thread") }
    default:
        return session.SessionOrigin{}, errors.New("Codex session has unknown thread source")
    }
    var source struct { Subagent *codexSubagentSource `json:"subagent"` }
    raw := bytes.TrimSpace(p.Source)
    if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &source) != nil || source.Subagent == nil {
        return session.SessionOrigin{}, errors.New("Codex subagent has invalid source provenance")
    }
    spawn := source.Subagent.ThreadSpawn
    guardian := source.Subagent.Other == "guardian"
    if (spawn != nil) == guardian || source.Subagent.Other != "" && !guardian {
        return session.SessionOrigin{}, errors.New("Codex subagent has ambiguous source provenance")
    }
    origin := session.SessionOrigin{ParentSessionID: p.ParentThreadID}
    if spawn != nil {
        if spawn.ParentThreadID != p.ParentThreadID { return session.SessionOrigin{}, errors.New("Codex subagent parent evidence conflicts") }
        origin.Kind, origin.AgentPath, origin.AgentName, origin.AgentRole = "thread_spawn", spawn.AgentPath, spawn.AgentNickname, spawn.AgentRole
    } else {
        origin.Kind = "guardian"
    }
    return origin, nil
}
```

- [ ] **Step 5: Write failing discovery tests for nesting, orphans, cycles, and duplicate IDs**

```go
func TestFormFamiliesGroupsCodexDescendantsUnderRoot(t *testing.T) {
    got, err := FormFamilies([]Candidate{codexCandidate("codex-root", ""), codexCandidate("codex-worker", "codex-root"), codexCandidate("codex-guardian", "codex-worker")}, testScope())
    if err != nil { t.Fatal(err) }
    if len(got) != 1 || len(got[0].Children) != 2 { t.Fatalf("families = %#v", got) }
    var guardian *ChildSourceCandidate
    for i := range got[0].Children {
        if got[0].Children[i].Candidate.SessionID == "codex-guardian" { guardian = &got[0].Children[i] }
    }
    if guardian == nil || guardian.ParentSessionID != "codex-worker" { t.Fatalf("guardian = %#v", guardian) }
}

func TestFormFamiliesDoesNotPromoteCodexOrphan(t *testing.T) {
    got, err := FormFamilies([]Candidate{codexCandidate("orphan", "missing")}, testScope())
    if err != nil || len(got) != 0 { t.Fatalf("families=%#v err=%v", got, err) }
}

func TestFormFamiliesExcludesInvalidCodexComponentsButKeepsValidRoots(t *testing.T) {
    candidates := []Candidate{codexCandidate("valid-root", "")}
    candidates = append(candidates, cyclicCodexCandidates()...)
    candidates = append(candidates, duplicateCodexCandidates()...)
    got, err := FormFamilies(candidates, testScope())
    if err != nil { t.Fatal(err) }
    if len(got) != 1 || got[0].ProviderSessionID != "valid-root" { t.Fatalf("families=%#v", got) }
}
```

- [ ] **Step 6: Form Codex components by ID rather than filename or time**

Add `Origin` to `Candidate` during `inspect`, then route Codex candidates through this graph boundary:

```go
func formCodexFamilies(candidates []Candidate, scope session.ProjectScope) ([]SessionFamilyCandidate, error) {
    counts := make(map[string]int, len(candidates))
    byID := make(map[string]Candidate, len(candidates))
    for _, candidate := range candidates {
        counts[candidate.SessionID]++
        byID[candidate.SessionID] = candidate
    }
    invalid := make(map[string]bool)
    for id, count := range counts {
        if count != 1 { invalid[id] = true }
    }
    for id, candidate := range byID {
        if parent := candidate.Origin.ParentSessionID; parent != "" && (counts[parent] != 1 || invalid[parent]) {
            invalid[id] = true
        }
    }
    markCodexCycles(byID, invalid)
    propagateInvalidCodexParents(byID, invalid)
    return buildCodexRootFamilies(byID, invalid, scope), nil
}
```

`markCodexCycles` uses white/gray/black visitation over `Origin.ParentSessionID` and marks every gray-loop member invalid instead of returning a global error. `propagateInvalidCodexParents` repeatedly marks descendants of missing or invalid parents until stable. `buildCodexRootFamilies` starts only from remaining candidates with an empty parent, recursively appends remaining descendants with their direct `ParentSessionID`, sorts children by session ID, and continues using the canonical root path for the family key. Orphan-only, duplicate-ID, and cyclic components are omitted. Add focused unit tests for both helpers so invalid descendants cannot leak back in through traversal.

- [ ] **Step 7: Run parser and discovery tests**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/parser ./internal/discovery -count=1`

Expected: PASS; guardian and worker rollouts are children, and no Codex subagent appears as a top-level candidate.

- [ ] **Step 8: Commit Codex family formation**

```bash
git add internal/parser internal/discovery
git commit -m "feat: group Codex rollout families by provenance"
```

### Task 4: Derive Complete Family Membership and Hosted Terminal Evidence

**Files:**
- Modify: `internal/session/model.go`
- Modify: `internal/session/validate.go`
- Modify: `internal/session/model_test.go`
- Modify: `internal/parser/parser.go:126-171`
- Modify: `internal/parser/claude.go`
- Modify: `internal/parser/parser_test.go`
- Modify: `internal/library/service.go:74-242`
- Modify: `internal/library/service_test.go`
- Modify: `internal/web/server_test.go:232-300`
- Modify: `internal/store/family.go`
- Modify: `internal/store/filesystem.go`
- Modify: `internal/store/s3.go`
- Modify: `internal/store/store_test.go`
- Modify: `internal/store/s3_test.go`

**Interfaces:**
- Consumes: parsed `Session.Origin`, `ChildSourceCandidate.ParentSessionID`, and `SessionFamily` graph validation.
- Produces: backward-compatible `Event.ResultStatus`, `AttachCodexChildren(main session.Session, children []session.Session) ([]session.ChildSession, error)`, and `deriveFamilyCompletion(session.SessionFamily, []session.SourceFactEntry, bool) (session.FamilyCompletion, error)`.

- [ ] **Step 1: Write failing Codex import and hosted completion tests**

```go
func TestImportCodexFamilyPersistsNestedMembers(t *testing.T) {
    st := store.NewFilesystem(t.TempDir())
    md, created, err := New(st, AllowLocalQuietEvidence()).ImportFamilyWithStatus(context.Background(), codexFamilySnapshot(t), attrs(t))
    if err != nil || !created { t.Fatalf("md=%#v created=%v err=%v", md, created, err) }
    got, err := st.GetSession(context.Background(), md.ID)
    if err != nil || len(got.Family.Children) != 2 { t.Fatalf("pkg=%#v err=%v", got, err) }
    var guardian *session.ChildSession
    for i := range got.Family.Children {
        if got.Family.Children[i].AgentID == "codex-guardian" { guardian = &got.Family.Children[i] }
    }
    if guardian == nil || guardian.ParentSessionID != "codex-worker" { t.Fatalf("guardian=%#v", guardian) }
}

func TestHostedUploadAcceptsAttachedChildOnlyWithTerminalParentResult(t *testing.T) {
    assertFamilyUploadStatus(t, completedParentResultMain(), nonterminalAttachedChild(), http.StatusCreated)
    assertFamilyUploadStatus(t, unknownParentResultMain(), nonterminalAttachedChild(), http.StatusUnprocessableEntity)
}

func TestHostedUploadNeverStoresIncompleteFamily(t *testing.T) {
    rr, st := postFamily(t, terminalMainWithoutChildProof(), nonterminalAttachedChild())
    if rr.Code != http.StatusUnprocessableEntity { t.Fatalf("status=%d", rr.Code) }
    if got := countStoredPackages(t, st); got != 0 { t.Fatalf("stored=%d", got) }
}

func TestClaudeParserPreservesParentResultStatus(t *testing.T) {
    got := parseClaude(t, parentResultFixture("completed"))
    result := eventByID(t, got, "result")
    if result.ResultStatus != "completed" { t.Fatalf("status=%q", result.ResultStatus) }
}

func TestStoreRejectsNewIncompleteFamilyButReadsExistingV2(t *testing.T) {
    // Seed the existing-v2 package through the test fixture/backdoor used by
    // store compatibility tests, then prove GetSession still accepts it.
    st, id := installExistingIncompleteV2(t)
    if _, err := st.GetSession(context.Background(), id); err != nil { t.Fatal(err) }
    if _, err := st.PutFamily(context.Background(), incompleteFamilyPackage(t)); err == nil {
        t.Fatal("stored a new incomplete family")
    }
}
```

Keep these helpers local to their owning test packages. `codexFamilySnapshot` opens the three Task 3 fixtures as snapshot inputs with `main`, `codex-worker`, and `codex-guardian` roles/agent IDs. The hosted helpers build a main Agent/Task call plus exactly one linked tool result; only the `completedParentResultMain` variant sets `toolUseResult.status=completed`. `installExistingIncompleteV2` writes the exact previous-v2 package files through existing low-level store fixture utilities, not through the strict public write path.

- [ ] **Step 2: Run focused tests and verify nested import or hosted policy fails**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/session ./internal/parser ./internal/library ./internal/web ./internal/store -run 'TestImportCodexFamily|TestHostedUploadAcceptsAttachedChildOnly|TestHostedUploadNeverStoresIncompleteFamily|TestClaudeParserPreservesParentResultStatus|TestStoreRejectsNewIncompleteFamily' -count=1`

Expected: FAIL because Codex children have no attachment path and attached nonterminal Claude children are currently persisted as `incomplete`.

- [ ] **Step 3: Construct Codex child entries from parsed origin**

```go
func AttachCodexChildren(main session.Session, children []session.Session) ([]session.ChildSession, error) {
    members := map[string]struct{}{main.ID: {}}
    for _, child := range children { members[child.ID] = struct{}{} }
    out := make([]session.ChildSession, 0, len(children))
    for _, child := range children {
        parent := child.Origin.ParentSessionID
        if parent == "" { return nil, errors.New("Codex child has no parent thread") }
        if _, ok := members[parent]; !ok { return nil, errors.New("Codex child parent is outside family") }
        out = append(out, session.ChildSession{
            AgentID: child.ID, ParentSessionID: parent, AgentType: child.Origin.Kind,
            Description: child.Origin.AgentPath, Session: child,
        })
    }
    sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
    return out, nil
}
```

Update Claude attachment to set `ParentSessionID=main.ID` and to preserve Agent/Task type and description from the proven parent tool-call input.

In `ImportFamilyWithStatus`, replace the current all-children-must-match-root identity check with provider rules:

```go
switch main.parsed.Provider {
case "claude":
    if child.parsed.ProviderSessionID != main.parsed.ProviderSessionID { return session.Metadata{}, false, errors.New("Claude family session mismatch") }
case "codex":
    if child.parsed.ProviderSessionID != child.parsed.ID { return session.Metadata{}, false, errors.New("Codex child identity mismatch") }
default:
    return session.Metadata{}, false, errors.New("provider does not support family children")
}
```

- [ ] **Step 4: Preserve parent-result status and derive Claude attachment metadata**

Add this optional field to `session.Event`, so schema-v1 and existing schema-v2 JSON continue to decode unchanged:

```go
ResultStatus string `json:"result_status,omitempty"`
```

Extend Claude `toolUseResult` decoding with:

```go
Status string `json:"status"`
```

Copy it only onto the normalized `EventToolResult`; unknown values remain preserved normalized evidence but are not terminal evidence.

When `AttachClaudeChildren` finds the single proven Agent/Task call, set `ParentSessionID=main.ID`, `ParentToolCallID`, `Attached`, and `AgentType` from the call name. Decode display-only call input with:

```go
var input struct {
    Description  string `json:"description"`
    SubagentType string `json:"subagent_type"`
}
```

Use `Description` as the child description and prefer non-empty `SubagentType` over the generic Agent/Task name for `AgentType`. Require exactly one matching result and exactly one matching Agent/Task call. When that result's `ResultStatus` is one of `completed`, `failed`, `cancelled`, or `interrupted`, set the copied child's completion to `Terminal=true` and `TerminalReason="parent_result_"+status`; unknown, missing, conflicting, or ambiguous results do not supply terminal evidence. Sort the final `[]ChildSession` by `AgentID` for both providers before deriving completion or building source facts.

- [ ] **Step 5: Centralize completion derivation and reject new incomplete families**

Implement completion derivation so each member is proven by its own/provider-parent terminal evidence or trusted local quiet evidence:

```go
func deriveFamilyCompletion(f session.SessionFamily, facts []session.SourceFactEntry, allowLocalQuiet bool) (session.FamilyCompletion, error) {
    quietByAgent := make(map[string]bool, len(facts))
    for _, fact := range facts { quietByAgent[fact.AgentID] = fact.Facts.QuietPeriodVerified }
    allTerminal := f.Main.Completion.Terminal
    usedQuiet := false
    if !f.Main.Completion.Terminal {
        if !allowLocalQuiet || !quietByAgent[""] { return session.FamilyCompletion{}, ErrIncomplete }
        usedQuiet = true
    }
    for _, child := range f.Children {
        if child.Session.Completion.Terminal { continue }
        allTerminal = false
        if !allowLocalQuiet || !quietByAgent[child.AgentID] { return session.FamilyCompletion{}, ErrIncomplete }
        usedQuiet = true
    }
    _, _, last := familyTimes(f)
    if allTerminal && !usedQuiet {
        return session.FamilyCompletion{Status: "provider_terminal", Reason: "all_members_terminal", LastEventAt: last}, nil
    }
    return session.FamilyCompletion{Status: "local_quiet", Reason: "verified_local_quiet", LastEventAt: last}, nil
}
```

Remove the branch that creates and persists `Status="incomplete"`. Hosted callers use `allowLocalQuiet=false`, so successful hosted imports are necessarily `provider_terminal`.

- [ ] **Step 6: Strengthen writes without breaking existing schema-v2 reads**

The current `validateFamilyPut` is also called by filesystem/S3 read and recovery paths. Do not make that shared function reject old `incomplete` packages. Split it into strict-write and compatible-read entry points:

```go
func validateFamilyPut(p session.Package) error  { return validateFamilyPackage(p, false) }
func validateFamilyRead(p session.Package) error { return validateFamilyPackage(p, true) }
```

Use `validateFamilyPut` from `PutFamily`, `familyFiles`, and identical-write checks. Use `validateFamilyRead` from filesystem/S3 package reads and recovery validation. Both validate source order, hashes, identities, limits, and source facts. Only the read path permits an already-stored `Completion.Status == "incomplete"` with its historical reason; new writes accept only the two derived states below.

In the compatibility regression, seed the old package with the existing filesystem/S3 low-level fixture helpers rather than calling the now-strict public `PutFamily`; the test must prove an immutable package written by the previous v2 implementation remains readable.

In the shared validator, require ordered facts and recompute the accepted completion states:

```go
func validateStoredCompletion(f session.SessionFamily, facts []session.SourceFactEntry, allowLegacyIncomplete bool) error {
    if len(facts) != len(f.Children)+1 || facts[0].Role != "main" || facts[0].AgentID != "" {
        return errors.New("family source facts order mismatch")
    }
    if allowLegacyIncomplete && f.Completion.Status == "incomplete" { return nil }
    allTerminal, allProven := f.Main.Completion.Terminal, f.Main.Completion.Terminal || facts[0].Facts.QuietPeriodVerified
    for i, child := range f.Children {
        fact := facts[i+1]
        if fact.Role != "child" || fact.AgentID != child.AgentID { return errors.New("family source facts order mismatch") }
        allTerminal = allTerminal && child.Session.Completion.Terminal
        allProven = allProven && (child.Session.Completion.Terminal || fact.Facts.QuietPeriodVerified)
    }
    if !allProven || f.Completion.Status == "incomplete" { return errors.New("family completion is unproven") }
    if allTerminal && (f.Completion.Status != "provider_terminal" || f.Completion.Reason != "all_members_terminal") { return errors.New("family terminal completion mismatch") }
    if !allTerminal && (f.Completion.Status != "local_quiet" || f.Completion.Reason != "verified_local_quiet") { return errors.New("family quiet completion mismatch") }
    return nil
}
```

- [ ] **Step 7: Run session, parser, library, web, and store tests**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/session ./internal/parser ./internal/library ./internal/web ./internal/store -count=1 -timeout=90s`

Expected: PASS; hosted storage contains no incomplete family and Codex descendants survive persistence.

- [ ] **Step 8: Commit membership and completion derivation**

```bash
git add internal/session internal/parser internal/library internal/web/server_test.go internal/store/family.go internal/store/filesystem.go internal/store/s3.go internal/store/store_test.go internal/store/s3_test.go
git commit -m "fix: derive complete family eligibility"
```

### Task 5: Snapshot Local Families Through Race-Safe Descriptors

**Files:**
- Create: `internal/discovery/safeopen.go`
- Create: `internal/discovery/safeopen_unix.go`
- Create: `internal/discovery/safeopen_linux.go`
- Create: `internal/discovery/safeopen_darwin.go`
- Create: `internal/discovery/safeopen_windows.go`
- Create: `internal/discovery/safeopen_unsupported.go`
- Create: `internal/discovery/safeopen_test.go`
- Modify: `internal/discovery/discovery.go:27-40,118-157,195-234`
- Modify: `internal/discovery/snapshot.go`
- Modify: `internal/discovery/discovery_test.go`
- Modify: `internal/library/service.go`
- Modify: `internal/cli/commands.go:649-690,884-905`
- Modify: `internal/web/handlers.go:562-600,678-693`

**Interfaces:**
- Consumes: configured provider root, root-relative candidate path, family source limit, and discovery identity.
- Produces: `safeOpen(root, relative string) (*os.File, fileIdentity, error)`, `SnapshotFamily(context.Context, SessionFamilyCandidate) (*FamilySnapshot, error)`, `SnapshotReaders(context.Context, SessionFamilyCandidate, []SnapshotInput) (*FamilySnapshot, error)`, `(*FamilySnapshot).Close() error`, and `SnapshotSource.Open() (io.ReadCloser, error)`.

- [ ] **Step 1: Write failing symlink, mutation, and cleanup tests**

```go
func TestSnapshotFamilyRejectsIntermediateSymlinkReplacement(t *testing.T) {
    roots, family, replace := symlinkReplacementFamily(t)
    replace()
    if _, err := SnapshotFamily(context.Background(), family); !errors.Is(err, ErrSourceChanged) { t.Fatalf("err=%v roots=%#v", err, roots) }
}

func TestSnapshotFamilyRejectsSameSizeInPlaceMutation(t *testing.T) {
    family, mutateBetweenPasses := mutatingFamily(t)
    snapshotTestHook = mutateBetweenPasses
    t.Cleanup(func() { snapshotTestHook = nil })
    if _, err := SnapshotFamily(context.Background(), family); !errors.Is(err, ErrSourceChanged) { t.Fatalf("err=%v", err) }
}

func TestFamilySnapshotCloseRemovesPrivateFiles(t *testing.T) {
    snapshot, err := SnapshotFamily(context.Background(), stableFamily(t))
    if err != nil { t.Fatal(err) }
    paths := snapshotTestPaths(snapshot)
    if err := snapshot.Close(); err != nil { t.Fatal(err) }
    for _, path := range paths { if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) { t.Fatalf("path remains: %s", path) } }
}

func TestSnapshotReadersRejectsTooManySourcesAndCancellation(t *testing.T) {
    inputs := make([]SnapshotInput, session.MaxFamilySources+1)
    for i := range inputs { inputs[i] = SnapshotInput{Role: "child", AgentID: fmt.Sprintf("a-%d", i), Reader: strings.NewReader("{}\n")} }
    inputs[0].Role, inputs[0].AgentID = "main", ""
    if _, err := SnapshotReaders(context.Background(), SessionFamilyCandidate{}, inputs); err == nil { t.Fatal("accepted too many sources") }
    ctx, cancel := context.WithCancel(context.Background())
    cancel()
    if _, err := SnapshotReaders(ctx, SessionFamilyCandidate{}, inputs[:1]); !errors.Is(err, context.Canceled) { t.Fatalf("err=%v", err) }
}
```

Implement `symlinkReplacementFamily`, `mutatingFamily`, and `stableFamily` with real files under `t.TempDir()`. The mutation hook fires after the first copy and before the second source hash. `snapshotTestPaths` exposes only application-created snapshot paths for cleanup assertions; it must never return original source paths.

- [ ] **Step 2: Run discovery tests and verify unsafe opening is exposed**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/discovery -run 'TestSnapshotFamilyRejectsIntermediateSymlinkReplacement|TestSnapshotFamilyRejectsSameSizeInPlaceMutation|TestFamilySnapshotCloseRemovesPrivateFiles' -count=1`

Expected: FAIL because `OpenEligible` uses `Lstat` plus `os.Open` and snapshots have no owned cleanup lifecycle.

- [ ] **Step 3: Introduce a platform-neutral safe-open contract**

```go
var ErrSafeOpenUnsupported = errors.New("safe source open unsupported")

type fileIdentity struct {
    Device, Inode uint64
    Size          int64
    ModTimeNS     int64
    ChangeTimeNS  int64
}

func sameIdentity(a, b fileIdentity) bool {
    return a.Device == b.Device && a.Inode == b.Inode && a.Size == b.Size && a.ModTimeNS == b.ModTimeNS && a.ChangeTimeNS == b.ChangeTimeNS
}
```

Support Linux and Darwin in `safeopen_unix.go`; keep their differing `Stat_t` timestamp extraction in `safeopen_linux.go` and `safeopen_darwin.go`. Open the configured root with `O_DIRECTORY|O_NOFOLLOW`, reject empty, absolute, `.` and `..` relative components, traverse every intermediate component with `openat(O_DIRECTORY|O_NOFOLLOW)`, open the final regular file with `O_NOFOLLOW`, and derive device, inode, size, mtime, and ctime from `fstat`. Close every intermediate descriptor on both success and error. Use build tags `linux || darwin`, `linux`, and `darwin` respectively.

On Windows, use `CreateFile` with `FILE_FLAG_OPEN_REPARSE_POINT|FILE_FLAG_BACKUP_SEMANTICS`, reject a reparse point at the configured root and at every component, and compare volume serial/file ID, size, last-write time, and change time from the opened handle. Use `//go:build windows`. The unsupported implementation uses `//go:build !linux && !darwin && !windows` and returns `ErrSafeOpenUnsupported`; it never falls back to `os.Open`.

- [ ] **Step 4: Replace absolute candidate authority with root-relative identity**

Add unexported authority fields and one reopening method:

```go
type Candidate struct {
    Path, Provider, SessionID, Project, Title, Status string
    StartedAt time.Time
    Origin session.SessionOrigin
    root, relativePath string
    identity fileIdentity
    quietVerified bool
}

func (c Candidate) openVerified() (*os.File, fileIdentity, error) {
    f, identity, err := safeOpen(c.root, c.relativePath)
    if err != nil { return nil, fileIdentity{}, err }
    if !sameIdentity(identity, c.identity) { f.Close(); return nil, fileIdentity{}, ErrSourceChanged }
    return f, identity, nil
}
```

`walk` obtains the initial discovery descriptor with `safeOpen`, parses that descriptor, and records the configured root, lexical relative path, and descriptor identity after rejecting `..`; the directory entry is not authority. `InspectPath` treats the explicit file's parent as its trusted root and the basename as its relative path. `DiscoverAllFamilies`, `familyForPath`, live rendering, and snapshotting call `openVerified`; none may reopen `Candidate.Path` with `os.Open`. Map a no-follow or identity mismatch after discovery to `ErrSourceChanged`, while returning `ErrSafeOpenUnsupported` unchanged.

- [ ] **Step 5: Create private snapshots and verify every source twice**

```go
type FamilySnapshot struct {
    Candidate SessionFamilyCandidate
    Sources   []SnapshotSource
    dir       string
    closeOnce sync.Once
    closeErr  error
}

type SnapshotSource struct {
    Role, AgentID string
    Facts         session.SourceFacts
    path          string
}

func (s SnapshotSource) Open() (io.ReadCloser, error) { return os.Open(s.path) }
func (s *FamilySnapshot) Close() error {
    s.closeOnce.Do(func() { s.closeErr = os.RemoveAll(s.dir) })
    return s.closeErr
}
```

`SnapshotFamily` creates a `0700` directory, creates each source as `0600`, hashes while copying from the safe descriptor under the family aggregate limiter, captures post-copy identity, rewinds and hashes the source descriptor a second time, captures final identity, and requires both source hashes and all identities to match. On any error it closes descriptors, removes the directory, and returns no snapshot.

Use one bounded copy primitive for local descriptors and multipart readers:

```go
type SnapshotInput struct { Role, AgentID string; Reader io.Reader; Facts session.SourceFacts }

func copySnapshotSource(ctx context.Context, dst *os.File, src io.Reader, remaining *int64) (string, int64, error) {
    h := sha256.New()
    n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(&contextReader{ctx: ctx, r: src}, *remaining+1))
    if err != nil { return "", 0, err }
    if n > *remaining { return "", 0, fmt.Errorf("family exceeds aggregate source limit") }
    *remaining -= n
    if err := dst.Sync(); err != nil { return "", 0, err }
    return hex.EncodeToString(h.Sum(nil)), n, nil
}

type contextReader struct {
    ctx context.Context
    r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
    if err := r.ctx.Err(); err != nil { return 0, err }
    return r.r.Read(p)
}
```

`contextReader.Read` returns `ctx.Err()` before delegating each read. For local sources, compare the copy hash with a second per-source-bounded hash of the rewound source descriptor and require pre-copy, post-copy, and post-hash identities to match; the verification pass does not decrement the aggregate budget a second time. Enforce `MaxFamilySources` before creating files. `SnapshotReaders` applies the same private-directory, `0600` file, aggregate-limit, cleanup, and cancellation behavior but has no filesystem identity check because the HTTP body reader is already the transport authority. Validate exactly one first `main` input and unique non-empty child agent IDs before copying.

- [ ] **Step 6: Parse only snapshot readers and close at every call site**

Change `ImportFamilyWithStatus` to accept `*discovery.FamilySnapshot`, call `SnapshotSource.Open`, and never read the original path. Change the single-source `Import` wrapper to construct a `SnapshotReaders` snapshot, defer `Close`, and delegate through the same pointer API. Update CLI and web import/render flows to use:

```go
snapshot, err := discovery.SnapshotFamily(ctx, family)
if err != nil { return err }
defer snapshot.Close()
_, _, err = service.ImportFamilyWithStatus(ctx, snapshot, attrs)
```

For focused live rendering, snapshot once per request and parse snapshot members. Multipart upload opens each part, passes its reader to `SnapshotReaders`, closes every multipart reader after copying, and returns the owned snapshot. For multi-selection local import, close all snapshots on the first snapshot/import failure and after the final import; add a regression that fails the second selected family and proves the first private snapshot was removed. No call site stores a `FamilySnapshot` value or leaves ownership implicit.

- [ ] **Step 7: Run discovery, library, CLI, and web tests**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/discovery ./internal/library ./internal/cli ./internal/web -count=1 -timeout=90s`

Expected: PASS for symlink substitution, in-place mutation, aggregate limits, cleanup, cancellation, explicit paths, local import, and focused browsing.

Compile the platform-specific boundary without executing foreign binaries:

```bash
mkdir -p .go-cache/platform-compile
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c ./internal/discovery -o .go-cache/platform-compile/discovery-linux.test
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c ./internal/discovery -o .go-cache/platform-compile/discovery-windows.test.exe
```

Expected: both compile commands exit 0; artifacts remain under the already ignored `.go-cache` tree.

- [ ] **Step 8: Commit safe snapshotting**

```bash
git add internal/discovery internal/library internal/cli internal/web
git commit -m "fix: snapshot local families without path races"
```

### Task 6: Render Nested Codex Descendants Without Flattening Claude Attachments

**Files:**
- Modify: `internal/review/review.go:20-58`
- Modify: `internal/review/review_test.go`
- Modify: `internal/web/handlers.go:750-825`
- Modify: `internal/web/templates/transcript.html`
- Modify: `internal/web/static/app.css`
- Modify: `internal/web/server_test.go`

**Interfaces:**
- Consumes: validated `ChildSession.ParentSessionID`, optional `ParentToolCallID`, and provider-derived agent metadata.
- Produces: recursive `review.TranscriptNode`, `review.ProjectFamily(session.SessionFamily) review.FamilyTranscript`, and recursive `childTranscriptView.Children`.

- [ ] **Step 1: Write failing nested-render tests**

```go
func TestProjectFamilyNestsGuardianUnderWorker(t *testing.T) {
    got := ProjectFamily(codexRootWorkerGuardianFamily())
    if len(got.Root.Children) != 1 || got.Root.Children[0].SessionID != "codex-worker" { t.Fatalf("root=%#v", got.Root) }
    if len(got.Root.Children[0].Children) != 1 || got.Root.Children[0].Children[0].SessionID != "codex-guardian" { t.Fatalf("worker=%#v", got.Root.Children[0]) }
}

func TestTranscriptRendersCodexGuardianOnceUnderWorker(t *testing.T) {
    body := renderFamily(t, codexRootWorkerGuardianFamily())
    if strings.Count(body, "Delegated work / codex-guardian") != 1 { t.Fatalf("body=%s", body) }
    if strings.Index(body, "Delegated work / codex-guardian") < strings.Index(body, "Delegated work / codex-worker") { t.Fatalf("guardian preceded worker: %s", body) }
}
```

Build `codexRootWorkerGuardianFamily` from a validated family with distinct root/worker/guardian session IDs and guardian parent `codex-worker`. `renderFamily` must execute the real embedded transcript template through `transcriptFamilyPage`; do not test a parallel hand-built renderer.

- [ ] **Step 2: Run review and web regressions and verify the graph is flattened**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/review ./internal/web -run 'TestProjectFamilyNestsGuardianUnderWorker|TestTranscriptRendersCodexGuardianOnceUnderWorker' -count=1`

Expected: FAIL because `FamilyTranscript` has only main-attached and one flat unattached list.

- [ ] **Step 3: Project the validated family into a recursive review tree**

```go
type TranscriptNode struct {
    SessionID, AgentID, ParentToolCallID, AgentType, Description string
    StartedAt time.Time
    Completion session.Completion
    Transcript Transcript
    Attached   map[string][]*TranscriptNode
    Children   []*TranscriptNode
}

type FamilyTranscript struct { Root *TranscriptNode }
```

Create all nodes first. The root is the only parent node for Claude; only distinct Codex child session IDs are added to the parent lookup. Normalize an empty `ParentSessionID` to the root ID before lookup so previously stored schema-v2 packages still render as direct children. Attach each child to its resolved parent node, then move children with a proven `ParentToolCallID` into that parent's `Attached` map. Sort both attached slices and remaining child slices by `StartedAt`, then `AgentID`, for deterministic output. Validation from Task 2 guarantees no recursive cycle.

- [ ] **Step 4: Render the tree recursively with stable family-scoped anchors**

Update `childTranscriptView` and convert recursively:

```go
type childTranscriptView struct {
    Anchor, SessionID, AgentID, AgentType, Description string
    Completion session.Completion
    Turns []turnView
    Diagnostics []eventView
    Attached map[string][]childTranscriptView
    Children []childTranscriptView
}

func childNodeView(node *review.TranscriptNode) childTranscriptView {
    view := childTranscriptView{Anchor: "child-"+node.AgentID, SessionID: node.SessionID, AgentID: node.AgentID, AgentType: node.AgentType, Description: node.Description, Completion: node.Completion, Attached: map[string][]childTranscriptView{}}
    for _, child := range node.Children { view.Children = append(view.Children, childNodeView(child)) }
    for callID, children := range node.Attached {
        for _, child := range children { view.Attached[callID] = append(view.Attached[callID], childNodeView(child)) }
    }
    return view
}
```

Preserve main prompt IDs and anchors so existing stored-session fragment links and the current `href="#main-prompt"` integration contract remain valid. Prefix only child prompt anchors with `child-<validated-agent-id>-` and use `child-<validated-agent-id>` for each child container; `ValidateFamily` guarantees family-wide unique child agent IDs. Keep normalized tool-call IDs unchanged because the recursive template indexes `Attached` by those IDs.

In `transcriptFamilyPage`, project `Root.Transcript` into the existing main `Turns` and `Diagnostics`, project `Root.Attached` into the main attachment map, and project `Root.Children` recursively. Inside the `child-transcript` template, bind the current node before ranging events (for example `{{$node := .}}`) and use `{{with index $node.Attached .ID}}` directly beneath the owning tool event. Render `node.Children` in that node's delegated-work section after its turns. Keep technical details collapsed and preserve `html/template` escaping.

- [ ] **Step 5: Run review and web tests**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/review ./internal/web -count=1`

Expected: PASS; root, worker, and guardian render once in ancestry order, while existing Claude attachment tests remain unchanged.

- [ ] **Step 6: Commit nested rendering**

```bash
git add internal/review internal/web
git commit -m "feat: render nested Codex delegated work"
```

### Task 7: Authorize Live Render and Import by Opaque Family Key

**Files:**
- Modify: `internal/discovery/family.go`
- Modify: `internal/web/handlers.go:31-85,450-569,603-685`
- Modify: `internal/web/server.go:73-105`
- Modify: `internal/web/templates/directory.html`
- Modify: `internal/web/server_test.go`
- Modify: `internal/cli/commands.go:563-595`
- Modify: `internal/cli/commands_test.go`

**Interfaces:**
- Consumes: `SessionFamilyCandidate.Key` and scope-aware `liveFamilies` rediscovery.
- Produces: `discovery.FamilyMap([]SessionFamilyCandidate) (map[string]SessionFamilyCandidate, error)` and scoped route `/live/families/<family-key>`.

- [ ] **Step 1: Write failing duplicate-session-ID selector tests**

```go
func TestScopedCatalogUsesDistinctFamilyKeysForDuplicateProviderSessionIDs(t *testing.T) {
    h, families := scopedServerWithDuplicateSessionIDs(t)
    body := getBody(t, h, "/live")
    for _, family := range families {
        if !strings.Contains(body, "/live/families/"+family.Key) { t.Fatalf("missing key %s: %s", family.Key, body) }
    }
}

func TestLiveImportUsesExactRediscoveredFamilyKey(t *testing.T) {
    h, families := scopedServerWithDuplicateSessionIDs(t)
    rr := postLiveImport(t, h, families[0].Key)
    if rr.Code != http.StatusSeeOther { t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String()) }
    assertOnlyFamilyImported(t, h, families[0])
}

func TestScopedFamilyRouteRejectsStaleAndPathTextKeys(t *testing.T) {
    h, _ := scopedServerWithDuplicateSessionIDs(t)
    assertStatus(t, h, "/live/families/f_"+strings.Repeat("0", 64), http.StatusNotFound)
    assertStatus(t, h, "/live/families/../../etc/passwd", http.StatusNotFound)
}
```

Build `scopedServerWithDuplicateSessionIDs` from two configured Claude roots that resolve to the same authorized project and contain root files with the same provider session ID but different canonical main paths. Do not use duplicate Codex IDs: Task 3 intentionally excludes those as an invalid graph component. This fixture proves the selector collision independently of Codex graph validation.
`postLiveImport` must issue a real local CSRF token/cookie pair and submit `family=<key>` through `/live/import`; `assertOnlyFamilyImported` verifies the stored source checksum/content identifies the chosen canonical main file, not merely that one package exists.

- [ ] **Step 2: Run focused web tests and verify provider/session collisions**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/web -run 'TestScopedCatalogUsesDistinctFamilyKeys|TestLiveImportUsesExactRediscoveredFamilyKey|TestScopedFamilyRouteRejectsStale' -count=1`

Expected: FAIL because the template, route, and import map still use `provider:sessionID`.

- [ ] **Step 3: Build a collision-checking key map after every rediscovery**

```go
var familyKeyPattern = regexp.MustCompile(`^f_[a-f0-9]{64}$`)

func FamilyMap(families []SessionFamilyCandidate) (map[string]SessionFamilyCandidate, error) {
    out := make(map[string]SessionFamilyCandidate, len(families))
    for _, family := range families {
        if !familyKeyPattern.MatchString(family.Key) { return nil, errors.New("invalid family key") }
        if _, exists := out[family.Key]; exists { return nil, errors.New("duplicate family key") }
        out[family.Key] = family
    }
    return out, nil
}
```

Use `discovery.FamilyMap` after every fresh authorized rediscovery for local GET `/live/families/<key>` and POST `/live/import`. A stale, malformed, absent, or non-unique key returns not found for GET and `selected session is no longer available` for POST; the CLI returns the map error. Never decode a key into a path. Preserve the existing all-projects route `/live/projects/<project-key>/families/<family-key>`, but make it use `FamilyMap` after filtering to the freshly rediscovered project so it has the same collision behavior.

- [ ] **Step 4: Pass families rather than flat candidates to the catalog template**

Add `Families []discovery.SessionFamilyCandidate` to `page`. Render checkbox values and links as:

```html
<input type="checkbox" name="family" value="{{.Key}}">
<a href="/live/families/{{.Key}}">{{.Title}}</a>
```

Remove `/live/<provider>/<sessionID>` from the scoped persistent catalog. Keep it only for a focused `browse` server whose selected candidate is already fixed in memory; do not use it as a rediscovered or persistent catalog selector. Existing stored `/sessions/<package-id>` routes are unchanged.

- [ ] **Step 5: Keep CLI `--family` on the same exact-hit contract**

Refactor selection to return an error for duplicate keys and never fall back:

```go
func selectFamily(families []discovery.SessionFamilyCandidate, key string, latest bool) (discovery.SessionFamilyCandidate, bool, error) {
    byKey, err := discovery.FamilyMap(families)
    if err != nil { return discovery.SessionFamilyCandidate{}, false, err }
    if latest && len(families) > 0 { return families[0], true, nil }
    family, ok := byKey[key]
    return family, ok, nil
}
```

Add a regression proving duplicate keys are an error and stale keys never fall back to provider/session ID or path matching.

- [ ] **Step 6: Run CLI and web tests**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/cli ./internal/web -count=1`

Expected: PASS for duplicate provider session IDs, stale/tampered keys, focused-server isolation, scoped catalog, CSRF, and existing stored-session routes.

- [ ] **Step 7: Commit opaque selector routing**

```bash
git add internal/discovery/family.go internal/cli internal/web
git commit -m "fix: select live families by opaque key"
```

### Task 8: Verify End-to-End Family Compatibility and Release Gates

**Files:**
- Modify: `internal/integration/roundtrip_test.go`
- Modify: `README.md`
- Modify: `docs/superpowers/plans/2026-07-16-session-family-local-browsing.md`

**Interfaces:**
- Consumes: Codex parent graph, safe snapshots, completion derivation, v2 storage, recursive review, and family-key routing from Tasks 1-7.
- Produces: one end-to-end regression proving discovery-to-render behavior and an explicit supersession note for the original v2 plan.

- [ ] **Step 1: Write the end-to-end Codex family regression**

```go
func TestCodexFamilyDiscoveryImportStoreAndRenderRoundTrip(t *testing.T) {
    roots, scope := installCodexRootWorkerGuardian(t)
    families, err := discovery.DiscoverFamilies(context.Background(), roots, scope, time.Now(), 5*time.Minute)
    if err != nil || len(families) != 1 || len(families[0].Children) != 2 { t.Fatalf("families=%#v err=%v", families, err) }

    snapshot, err := discovery.SnapshotFamily(context.Background(), families[0])
    if err != nil { t.Fatal(err) }
    defer snapshot.Close()

    st := store.NewFilesystem(t.TempDir())
    attrs := library.ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "local"}, UploaderKey: "local"}
    md, created, err := library.New(st, library.AllowLocalQuietEvidence()).ImportFamilyWithStatus(context.Background(), snapshot, attrs)
    if err != nil || !created { t.Fatalf("md=%#v created=%v err=%v", md, created, err) }
    pkg, err := st.GetSession(context.Background(), md.ID)
    if err != nil || len(pkg.Family.Children) != 2 { t.Fatalf("pkg=%#v err=%v", pkg, err) }
    var guardian *session.ChildSession
    for i := range pkg.Family.Children {
        if pkg.Family.Children[i].AgentID == "codex-guardian" { guardian = &pkg.Family.Children[i] }
    }
    if guardian == nil || guardian.ParentSessionID != "codex-worker" { t.Fatalf("guardian=%#v", guardian) }
    handler := web.New(web.ServerConfig{Store: st})
    rr := httptest.NewRecorder()
    handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+md.ID, nil))
    if rr.Code != http.StatusOK { t.Fatalf("render status=%d body=%s", rr.Code, rr.Body.String()) }
    body := rr.Body.String()
    for _, text := range []string{"Root prompt", "Worker prompt", "Guardian review"} {
        if strings.Count(body, text) != 1 { t.Fatalf("%q count mismatch: %s", text, body) }
    }
}

func installCodexRootWorkerGuardian(t *testing.T) (discovery.Roots, session.ProjectScope) {
    t.Helper()
    project := t.TempDir()
    logs := t.TempDir()
    for _, name := range []string{"codex-family-main.jsonl", "codex-family-worker.jsonl", "codex-family-guardian.jsonl"} {
        raw, err := os.ReadFile(filepath.Join("..", "parser", "testdata", name))
        if err != nil { t.Fatal(err) }
        raw = bytes.ReplaceAll(raw, []byte("/repo"), []byte(project))
        target := filepath.Join(logs, "rollout-"+name)
        if err := os.WriteFile(target, raw, 0o600); err != nil { t.Fatal(err) }
        old := time.Now().Add(-10 * time.Minute)
        if err := os.Chtimes(target, old, old); err != nil { t.Fatal(err) }
    }
    scope, err := discovery.ResolveProjectScope(project)
    if err != nil { t.Fatal(err) }
    return discovery.Roots{Codex: []string{logs}}, scope
}
```

- [ ] **Step 2: Run the integration test**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/integration -run '^TestCodexFamilyDiscoveryImportStoreAndRenderRoundTrip$' -count=1`

Expected: PASS with one root family, two nested descendants, one atomic package, and one rendered occurrence per prompt.

- [ ] **Step 3: Update operator documentation and supersede the incomplete v2 assumptions**

Add this behavior to `README.md`:

```markdown
Codex parent sessions and their worker, reviewer, and guardian rollouts are
shown as one nested family when Codex records explicit parent-thread
provenance. Orphaned subagent rollouts are not promoted into the catalog.
```

Add this note immediately below the original v2 plan heading:

```markdown
> **Remediation:** `2026-07-17-v2-session-family-remediation.md` supersedes
> this plan's Codex one-source assumption, local source-opening algorithm,
> hosted completion derivation, S3 reclaim retry behavior, and persistent
> live selector wiring.
```

- [ ] **Step 4: Run the full release gate**

Run: `GOCACHE="$PWD/.go-cache" go test ./... -count=1 -timeout=180s`

Expected: PASS with no package timeout or failure.

Run: `GOCACHE="$PWD/.go-cache" go test -race ./internal/discovery ./internal/library ./internal/store ./internal/web -count=1 -timeout=240s`

Expected: PASS with no race report.

Run: `GOCACHE="$PWD/.go-cache" go vet ./...`

Expected: exit 0 with no diagnostics.

Run: `git diff --check`

Expected: no output.

- [ ] **Step 5: Commit the release gate and documentation**

```bash
git add internal/integration/roundtrip_test.go README.md docs/superpowers/plans/2026-07-16-session-family-local-browsing.md
git commit -m "test: verify complete v2 session families"
```

## Plan Self-Review

- **Spec coverage:** Task 1 repairs the confirmed S3 response-loss hang. Tasks 2-4 replace the incorrect Codex one-source assumption, isolate invalid graph components, preserve normalized parent-result status, retain nested membership through import/storage, and enforce hosted terminal evidence. Task 5 implements the missing descriptor-safe private snapshot boundary with Linux, Darwin, Windows, and explicit unsupported-platform behavior. Task 6 renders nested descendants without changing main prompt anchors. Task 7 removes provider/session selector collisions. Task 8 proves end-to-end behavior and runs the release gates.
- **Scope:** The work remains one remediation wave because every task contributes to the same session-family correctness and release boundary. No deployment, authentication, unrelated parser enrichment, or visual redesign is included.
- **Backward compatibility:** Existing schema-v1 packages, legacy one-source Codex logs without `thread_source`, schema-v2 packages with empty `ParentSessionID`, and previously stored v2 `incomplete` packages remain readable. Strict write validation prevents creating new incomplete packages. Stored `/sessions/<package-id>` URLs and main prompt fragments remain unchanged.
- **Placeholder scan:** Every task names exact files, interfaces, failing tests, implementation behavior, commands, expected results, and commit boundaries; no deferred implementation markers remain.
- **Type consistency:** `SessionOrigin.ParentSessionID` flows from parser to `Candidate.Origin`; `ChildSourceCandidate.ParentSessionID` flows from discovery into `ChildSession.ParentSessionID`; `Event.ResultStatus` carries Claude terminal-result evidence into attachment; review consumes the validated relationship with legacy-empty parent normalization. `SnapshotFamily` and `SnapshotReaders` consistently return `*FamilySnapshot`, and all consumers close it.
