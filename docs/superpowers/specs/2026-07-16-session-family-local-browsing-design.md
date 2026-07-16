# Agent Transcripts v2: Project-Scoped Session Families

**Date:** 2026-07-16
**Status:** Design complete, pending user review
**Scope:** Local project discovery, complete provider-session parsing, focused browsing, family persistence, and compatible hosted publication

## Goal

`agent-transcripts` must present one complete agent session as one reviewable record. By default, it operates on the Git repository in which the command starts, asks the user to select one session, and opens only that session. Cross-project browsing is available only through `--all-projects`.

A complete session includes the main agent transcript and delegated Claude subagent transcripts. Codex provider records that duplicate one user interaction must produce one visible prompt. Raw provider evidence remains available for diagnostics without becoming a false user turn.

## User Promise

From any directory inside a Git worktree, running `agent-transcripts` shows recent completed sessions for that worktree, lets the user choose one, and opens a focused, server-rendered transcript. The transcript contains the main conversation and collapsible delegated work attached to the parent Agent or Task call by stable provider identity.

The product never silently drops a discovered child source, guesses a parent relationship from prose, mixes another project into the selected family, or persists only part of a family.

## Evidence and Existing Failure Modes

### Codex duplicate prompts

Observed Codex rollouts record the same real user instruction twice at the same interaction boundary:

- `response_item` with `payload.type = "message"` and `payload.role = "user"`
- `event_msg` with `payload.type = "user_message"`

The current parser maps both records to `EventUser`. The review projection starts a new turn for every `EventUser`, so one user interaction becomes two prompt cards. The same `response_item` stream also contains injected environment and skill envelopes that are provider context, not user-authored turns.

### Claude session fragmentation

Claude stores a main session as `<session-id>.jsonl` and delegated sessions beneath `<session-id>/subagents/agent-<agent-id>.jsonl`. The current recursive discovery treats every JSONL file as an independent candidate. It therefore lists children as top-level sessions and cannot render the parent and children as one record.

Real provider records contain a stable linkage chain:

```text
child record agentId
  -> parent toolUseResult.agentId
  -> parent tool_result.tool_use_id
  -> parent Agent or Task tool_use.id
```

The child metadata description is useful display text but is not an identity. Equal descriptions, retries, and renamed tasks make prose matching unsafe.

### Unscoped local catalog

The current discovery API scans all configured Claude and Codex roots. The CLI and web server do not pass a current-project constraint, and live selectors use `provider:sessionID`, which is neither path-specific nor collision-safe.

### Single-source persistence

The current package contains one `Session`, one raw `Source`, one normalized document, and a content ID derived from one source checksum. A main-plus-children family cannot be represented or written atomically with this model.

## Considered Approaches

### Option 1: Patch visible symptoms

Suppress one Codex record shape, exclude `agent-*` files from Claude discovery, and keep the existing single-session package.

This is small, but it does not deliver the complete-session promise. Claude child evidence disappears instead of being attached, response-only Codex logs can lose prompts, and project scoping remains ambiguous.

### Option 2: Build a generic cross-provider event graph

Convert every provider record into graph nodes and infer conversations, branches, and file relationships through one general graph engine.

This could support future providers, but the current provider contracts do not justify the abstraction. It would make parsing, persistence, and error handling harder to reason about before the product has stable family semantics.

### Option 3: Provider-aware session families

Resolve a project first, discover one main source plus provider-defined child sources, normalize each source independently, and project the result into a provider-neutral `SessionFamily`. Persist the family and its ordered source manifest as one package.

**Decision:** Option 3. It fixes the observed root causes at their boundaries while keeping provider quirks in provider adapters. A Codex family currently has one main source. A Claude family has one main source and zero or more stable-identity children. The model can gain other provider relationships later without a speculative graph engine.

## Command and Navigation Contract

### Focused browse is the default

Running `agent-transcripts` with no subcommand is an alias for:

```text
agent-transcripts browse
```

`browse`:

1. resolves the current project scope;
2. discovers completed session families in that scope;
3. shows an interactive single-selection picker ordered newest first;
4. starts a loopback-only HTTP server scoped to the selected family;
5. opens the selected family route unless `--no-open` is supplied; and
6. serves until interrupted.

The focused server exposes the selected live transcript and static assets. It does not expose a live catalog, another family, or another project.

Non-interactive use requires one of:

- `browse --family <opaque-family-key>` for a family returned by machine-readable discovery;
- `browse <path>` for an explicit eligible main source, with native children discovered from that main source's adjacent provider layout; or
- `browse --latest` to choose the newest eligible family in scope.

### Persistent catalog remains explicit

`agent-transcripts serve` retains the persistent local or hosted catalog behavior. In local mode its live catalog is current-project scoped by default. `serve --all-projects` enables the project index and cross-project live catalog. Existing library and hosted routes remain available.

### Import follows the same scope

`agent-transcripts import` discovers only the current project by default. `import --all-projects` first selects a project and then one or more families. Explicit paths continue to bypass discovery scope but not eligibility, identity, size, or parsing validation.

### Cross-project access is opt-in

`--all-projects` is supported by `browse`, `serve`, and `import`. Interactive flows select a project before a family. The browser project index is not reachable from a focused `browse` server; it is available only when the persistent server started with `--all-projects`.

## Project Identity and Scope

Project resolution separates the persisted reference from the local discovery scope:

```go
type ProjectRef struct {
    Kind        string `json:"kind"` // git_worktree, directory, or unresolved
    Key         string `json:"key"`  // p_<sha256(kind, canonical-root)>
    DisplayName string `json:"display_name"`
}

type ProjectScope struct {
    Ref           ProjectRef
    CanonicalRoot string // local discovery only; this type has no JSON form
}
```

The opaque key is used for equality. Only `ProjectRef` can appear in `SessionFamily`, metadata, or normalized JSON. `ProjectScope` is confined to discovery and authorization APIs and has no serializer. Stored normalized families retain the kind, key, and display name plus the provider's already-recorded working-directory provenance; they do not add the canonical root or discovery source path.

### Current project resolution

The resolver:

1. obtains the process working directory;
2. cleans it and resolves existing symlinks;
3. runs Git root discovery for that path; and
4. uses the canonical `git rev-parse --show-toplevel` result as the project key.

A linked Git worktree is its own default project scope. The resolver does not collapse it to the common Git directory or primary checkout. Sessions from sibling worktrees are visible only with `--all-projects`.

If the current directory is not inside Git, the canonical cleaned directory becomes a labelled `directory` scope. Only sessions whose recorded canonical working directory equals or is contained by that directory match. The picker states that Git identity was unavailable.

### Session project resolution

Discovery derives membership from parsed provider CWD records, not solely from Claude's encoded directory name.

- If a recorded CWD exists, resolve symlinks and its Git worktree root.
- If it exists outside Git, use its canonical cleaned directory.
- If it no longer exists, preserve the cleaned absolute path as an `unresolved` project identity.
- Claude's encoded project folder may narrow the files inspected, but cannot establish identity because dash encoding is ambiguous.

The main source determines family scope. Every child with a non-empty CWD must resolve to the same project key as the main source. A conflicting child makes the family invalid. Missing child CWD inherits the verified parent project only after stable agent linkage succeeds.

If records within the main source resolve to multiple Git roots, discovery marks the family invalid instead of choosing the first CWD. Directory changes within one Git worktree are retained as provenance and do not split the family.

## Discovery and Selection Model

### `SessionFamilyCandidate`

Discovery returns a family candidate rather than a flat file candidate:

```go
type SessionFamilyCandidate struct {
    Key               string
    Provider          string
    ProviderSessionID string
    Project           ProjectRef
    Title             string
    StartedAt         time.Time
    Status            string
    Main              SourceCandidate
    Children          []ChildSourceCandidate
}
```

`SourceCandidate` retains the already-openable path plus file identity, size, modification time, completion evidence, and parsed discovery facts. `ChildSourceCandidate` additionally carries the child `AgentID` found in provider records.

### Family formation

For Claude:

- A root-level `<session-id>.jsonl` is a main candidate.
- Only `<session-id>/subagents/agent-*.jsonl` beneath that exact main basename can be its children.
- Within native local Claude roots, the main filename is the authoritative provider session ID.
- Every non-empty record `sessionId` in the main or child must equal the main filename ID.
- Every child must contain one consistent non-empty `agentId`; the filename is checked against it but is not trusted as the identity source.
- Orphan `subagents` directories and mixed-session or mixed-agent files are invalid and never become top-level candidates.

For Codex, each rollout file is a one-source family. A future multi-file Codex relationship requires explicit provider evidence and a schema revision.

### Opaque family keys

The live selector key is:

```text
f_<sha256(provider, canonical-main-path)>
```

The route never decodes a key into a path. It rediscovers the authorized scope, builds a key-to-candidate map, and accepts only an exact map hit. Duplicate keys are a hard discovery error. The key is a local selector, not the stored content ID and not an authorization credential.

## Normalized Family Model

Imports created by v2 use normalized schema version 2:

```go
type SessionFamily struct {
    SchemaVersion     int
    ID                string
    Provider          string
    ProviderSessionID string
    Project           ProjectRef
    Main              Session
    Children          []ChildSession
    StartedAt         time.Time
    EndedAt           time.Time
    Completion        FamilyCompletion
}

type FamilyCompletion struct {
    Status        string // provider_terminal, local_quiet, or incomplete
    Reason        string
    LastEventAt   time.Time
}

type ChildSession struct {
    AgentID          string
    ParentToolCallID string
    AgentType        string
    Description      string
    Attached         bool
    Session          Session
}
```

Children are sorted by `AgentID` in normalized storage. Rendering may order attached children by the parent tool-call position and unattached children by start time, then `AgentID`.

`SessionFamily.ID` is the provider session ID for normalized semantics. Stored package and content IDs remain application-managed digests.

Family semantic fields are derived, never supplied independently:

- `StartedAt` is the earliest non-zero member `StartedAt`.
- For each member, its effective end is `EndedAt` when present, otherwise `Completion.LastEventAt`. Family `EndedAt` is the latest non-zero effective end.
- `Completion.LastEventAt` equals the latest member `Completion.LastEventAt`.
- `Completion.Status = provider_terminal` and `Reason = all_members_terminal` only when the main has provider terminal evidence and every child has either its own provider terminal evidence or a recognized terminal parent Agent/Task result.
- Recognized parent-result terminal states are adapter-owned explicit values such as completed, failed, cancelled, and interrupted. Unknown or absent statuses are not terminal.
- `Completion.Status = local_quiet` and `Reason = verified_local_quiet` when the family is not provider-terminal but every remaining member has trusted local quiet evidence.
- Otherwise the status is `incomplete` with reason `member_incomplete`, and the family cannot be previewed, imported, uploaded, or persisted.

`ValidateFamily` recomputes these values and rejects mismatches, a family end before its start, provider or session-ID disagreement, duplicate child agent IDs, attached children without an existing parent tool-call ID, unattached children with a parent tool-call ID, or child project disagreement. Local packages may persist `provider_terminal` or `local_quiet`; hosted upload accepts only `provider_terminal`.

Existing schema-v1 packages remain readable. The read path projects a v1 `Session` as a v2 family with `Main` populated and no children. New imports always write schema v2; no in-place migration rewrites existing immutable packages.

## Provider Normalization

### Codex interaction deduplication

The parser first creates provider records and turn boundaries, then decides visibility. It does not append visible user events independently while scanning.

Within one turn boundary:

1. Every `event_msg/user_message` is a canonical user interaction.
2. A preceding unmatched `response_item/message/user` with exactly equal decoded text in the same boundary is its paired duplicate and remains raw diagnostic evidence.
3. Provider-generated response-item envelopes with a recognized complete top-level structure such as `environment_context` or `skill`, appearing in the provider-context position before the canonical user interaction, remain diagnostic evidence.
4. An unpaired, non-synthetic response-item user message remains visible. This preserves response-only and partially recorded formats.
5. Exact repeated user messages in different turn boundaries remain distinct interactions.

Where a Codex version provides explicit turn IDs, those IDs define the boundary. Otherwise the adapter uses ordered `turn_context`, `task_complete`, and next-user transitions. Pairing never crosses a boundary and never relies on an arbitrary wall-clock tolerance.

The normalized event records its provider source kind so tests and technical details can show why one record was visible and its pair was not. Raw paired and synthetic records are not included in the prompt index.

### Claude record normalization

The Claude adapter preserves provider record identity before projecting content:

- The main filename defines the family session ID; inconsistent non-empty `sessionId` values are an error.
- Child `agentId`, `isSidechain`, `parentUuid`, message ID, summary markers, and compaction markers are parsed explicitly.
- Adjacent assistant records with the same provider message ID and compatible parent chain are one streaming response. Text uses the latest provider snapshot, while tool blocks are unioned by stable block ID in first-seen order.
- Repeated block IDs with conflicting type or payload make the source invalid; they are not silently merged.
- `summary` and `isCompactSummary` records are retained as compaction diagnostics and boundaries, not user prompts.
- Sidechain user records inside a child remain child interactions. They never create top-level family candidates.
- All non-empty per-record CWD values are retained and checked under the project policy.

Unknown record and block types remain raw diagnostic events. They do not abort parsing unless they violate identity, size, JSON, or linkage invariants.

## Claude Child Attachment

Attachment uses stable provider identity only:

1. Parse the child `AgentID` from its records.
2. Find exactly one parent Agent or Task completion whose `toolUseResult.agentId` equals it.
3. Read the corresponding parent tool-result block's `tool_use_id`.
4. Find exactly one parent `Agent` or `Task` tool call whose block ID equals that `tool_use_id`.
5. Set `ParentToolCallID` and render the child beneath that call.

The parent tool name may be `Agent` or `Task`; identity, not the name, establishes the relationship.

If the child is structurally part of the family but the stable chain is absent or ambiguous, retain it as `Attached = false` under a collapsed **Unattached delegated work** section. A description is display metadata only. Duplicate descriptions never influence attachment.

An `agentId` that maps to multiple parent results, multiple tool calls, conflicting child files, or another project is an invalid family rather than an unattached child. This prevents identity spoofing and cross-project disclosure.

## Completion and Atomic Eligibility

A family is eligible only when it is a stable snapshot:

- the main source satisfies existing terminal or trusted local quiet-period rules;
- every discovered child is parseable, within limits, identity-consistent, and no longer changing;
- an attached child's completed parent Agent or Task result is terminal evidence for that child's delegated run;
- an unattached child requires its own terminal evidence or trusted local quiet-period evidence; and
- hosted upload never trusts client quiet-period facts.

Discovery may display an ineligible family as **still active** in persistent local catalog mode, but `browse`, preview, import, and upload reject it. Focused browse selection lists eligible families only.

Opening or importing a family follows one transaction boundary:

1. safely open every discovered source through the platform strategy below;
2. copy every descriptor into a private bounded snapshot while hashing it;
3. verify every snapshot against discovery identity and a second stable read;
4. parse and validate only the private snapshots;
5. build and validate the complete family and source manifest; and
6. call one store operation for one package.

If any step fails, close and discard every snapshot and persist nothing.

### Race-safe source snapshot

Local source opening cannot use a plain `Lstat` followed by `os.Open`. On Unix, the implementation opens the configured provider root as a directory descriptor, walks each relative component with descriptor-relative `openat` plus `O_NOFOLLOW` (and `O_DIRECTORY` for intermediate components), opens the final regular file with `O_NOFOLLOW`, and verifies device/inode identity with `fstat`. On Windows, it walks handles with reparse-point opening enabled and rejects every reparse point before verifying the final file ID. A platform without an equivalent no-follow and file-identity implementation returns `safe source open unsupported`; it does not fall back to an unsafe open.

For each safely opened descriptor:

1. record device/file ID, size, nanosecond modification time, and change time;
2. copy through the aggregate limiter into an application-created `0700` temporary directory and `0600` file while computing SHA-256;
3. rewind the source descriptor and hash the entire bounded source again without writing;
4. `fstat` again and require all recorded identity/change facts to match; and
5. require the second source hash to equal the private snapshot hash.

Only the private snapshot is parsed or persisted. Once the two-pass verification succeeds, later source mutations cannot change the imported bytes. Temporary names are application-generated, temporary files are never reopened through user-controlled paths, and cleanup occurs on every error. Multipart uploads use the same private snapshot and aggregate limiter after the HTTP body limit, without filesystem source-path operations.

## Composite Source and Persistence Contract

### Limits

`MaxSourceBytes` becomes the maximum aggregate uncompressed bytes across the main and every child source in a family. `MaxRecordBytes` remains per JSONL record. The source count is capped at 256, including the main source, to bound descriptors, manifest size, and parser work.

Hosted multipart requests install an aggregate body limit before parsing. Repeated child parts cannot multiply the effective 64 MiB source limit.

### Source manifest

Every v2 package contains a canonical manifest:

```go
type SourceManifest struct {
    SchemaVersion int
    Provider      string
    SessionID     string
    Sources       []SourceEntry
}

type SourceEntry struct {
    Role     string // main or child
    AgentID  string // empty for main
    Checksum string
    Bytes    int64
    Name     string // canonical package-relative name
}
```

The main entry is first. Child entries are sorted by `AgentID`. Canonical names are `source/main.jsonl` and `source/children/<validated-agent-id>.jsonl`. Original absolute paths and untrusted upload filenames are not stored in the manifest.

The content ID is the digest of the provider plus the canonical JSON encoding of every ordered entry's role, agent ID, checksum, and byte length. The package ID remains the content ID plus destination. Changing, adding, or removing a source changes the content ID. Input enumeration order does not change it because canonical child order is always `AgentID` order.

### Package and store

The v2 package replaces singular raw and normalized fields with:

```go
type Package struct {
    SchemaVersion  int
    ID             string
    ContentID      string
    Family         SessionFamily
    Metadata       Metadata
    SourceManifest SourceManifest
    Sources        []SourceBlob
    Normalized     []byte
    SourceFacts    []SourceFactEntry
}

type SourceFactEntry struct {
    Role    string
    AgentID string
    Facts   SourceFacts
}
```

`SourceBlob` contains a validated manifest entry and its bounded immutable bytes. `SourceFactEntry` identifies the facts by source role and agent ID rather than relying on slice position. No source path is part of the stored package.

Schema-v2 metadata adds `SourceManifestChecksum`. The singular `SourceChecksum` remains part of the schema-v1 wire format only. Store readers decode either wire format and return the in-memory family form; a v1 package is converted to a one-main-source manifest without rewriting stored files.

`PutFamily` writes all source blobs, normalized family JSON, metadata, and the source manifest into one temporary package directory. It verifies hashes, fsyncs files and directories, renames the directory, and creates the final package manifest last, following the existing create-if-absent pattern. S3 uses immutable object keys and publishes the final package manifest only after every referenced object exists. Readers treat a package without the final manifest as nonexistent.

`PutSession` remains as a compatibility adapter that constructs a one-main-source family. Import services parse and validate all sources before invoking `PutFamily`; they never loop over children and call the store separately.

### Hosted upload

The multipart upload contract accepts exactly one `source` main file and zero or more `child` file parts. Upload filenames are untrusted and do not establish session or agent identity. The server derives the provider session ID from a consistent non-empty session ID in the main records, then requires every child's record session ID to match. The server does not accept a client-supplied normalized family, source manifest, agent ID, checksum, project identity, or attachment mapping. It derives all of those from bounded source bytes and provider records.

The existing single `source` request remains a valid one-source family upload. The upload client reads the stored package and sends its main and child blobs. Hosted parsing requires provider terminal evidence for the main source; matched completed Agent or Task results provide child terminal evidence. An unattached child without provider terminal evidence makes the hosted upload ineligible.

## Review Projection and Rendering

`review.ProjectFamily` produces:

- main turns and main technical diagnostics;
- attached children keyed by parent tool-call event ID; and
- unattached children as a separate collection.

The prompt index lists main user prompts only. Within a main turn, an Agent or Task tool card shows a collapsed child transcript. Each child has its own prompt index, event stream, completion status, agent type, and description. Nested content uses server-rendered `<details>` and stable anchors and works without JavaScript. JavaScript may add copy-link and disclosure conveniences only.

Technical details preserve raw provider evidence, including paired Codex records, compaction records, and unknown blocks. In hosted mode they inherit the package's existing authorization. A child is never separately addressable outside its owning family, preventing a selector from exposing child source bytes under another project or destination.

Templates continue to escape all provider content. Content Security Policy, CSRF controls, local loopback binding, authentication, ownership checks, and mutation revision checks remain unchanged.

## Error Handling

User-facing errors identify the failed boundary without including source content, tool arguments, absolute paths, tokens, or raw provider payloads:

- no project scope could be resolved;
- no eligible completed families in scope;
- selected family is no longer available;
- family changed after discovery;
- family exceeds aggregate size or source-count limits;
- provider identity or child linkage is inconsistent;
- family contains sources from different projects;
- family is incomplete; or
- package persistence failed.

Detailed local logs may include a redacted source role and stable family key, but not raw content. Hosted responses remain generic for malformed or unauthorized input.

Malformed optional diagnostics do not hide otherwise valid records. Identity, JSON, size, completion, cross-project, and conflicting-block failures invalidate the family because continuing could produce a misleading or unsafe record.

## Verification Matrix

### Codex parser

- paired response-item and event-message records produce one visible prompt;
- the pair remains available as diagnostic evidence;
- response-only user records remain visible;
- environment and skill envelopes do not enter the prompt index;
- equal text in different turn boundaries remains two interactions;
- equal text inside one boundary but without a canonical pair is not accidentally removed; and
- partial, unknown, and legacy turn boundaries use the documented fallback without panics.

### Claude parser and attachment

- stable `agentId -> toolUseResult -> tool_use_id -> Agent/Task` chains attach the correct child;
- both `Agent` and `Task` tool names work;
- duplicate descriptions attach by ID, not prose;
- an absent stable chain produces an unattached child;
- duplicate agent IDs, conflicting parent results, and cross-project children reject the family;
- streaming assistant snapshots merge once by message and block ID;
- conflicting repeated block IDs reject the source;
- summaries and compact summaries remain diagnostics;
- sidechains stay inside their child transcript;
- mixed session IDs reject a source; and
- per-record CWD changes inside one repo are retained while cross-repo changes reject the family.

### Discovery and project scope

- launching from the repo root and a nested directory returns the same families;
- a symlinked launch path resolves to the canonical worktree root;
- linked worktrees remain distinct default scopes;
- non-Git fallback is labelled and directory-scoped;
- missing historical CWDs remain unresolved and are excluded from an unrelated default scope;
- serialized `ProjectRef`, family JSON, and metadata contain no `CanonicalRoot` field and cannot serialize `ProjectScope`;
- Claude child files never appear as top-level candidates;
- `--all-projects` chooses a project before sessions; and
- default discovery never returns another project's source bytes.

### Eligibility, limits, and atomicity

- main or child mutation after discovery rejects the family;
- descriptor-relative no-follow opening rejects an intermediate or final-component symlink substitution;
- an `Lstat`-to-open replacement cannot select a different file because selection is rooted in directory descriptors and verified file identity;
- same-inode, same-size, restored-mtime mutation changes change-time or the second-pass hash and is rejected;
- malformed, incomplete, or unreadable children cause zero persistence;
- aggregate size and 256-source limits apply before full allocation;
- symlink swaps and path replacement are rejected for every member;
- deterministic source ordering yields a stable content ID;
- adding, removing, or changing a child changes the content ID;
- filesystem and S3 readers cannot observe partial packages; and
- repeated identical family imports remain idempotent.

### Family semantics

- a child that starts before the main determines family `StartedAt`;
- a child that ends after the main determines family `EndedAt` and `LastEventAt`;
- an attached child with a recognized terminal parent result contributes provider-terminal evidence;
- an unattached quiet child yields `local_quiet` locally and is rejected by hosted upload;
- an unknown parent result status leaves the family incomplete;
- recomputed timestamps or completion values that differ from stored values fail validation; and
- v1 one-source projection derives the same timestamps and equivalent completion status as its original session and source facts.

### Selectors and web behavior

- opaque family keys resolve only through the current authorized discovery map;
- selector tampering, collision, path traversal text, and stale keys return not found;
- focused browse exposes only its selected family and static assets;
- persistent serve defaults to current project and enables project routes only with `--all-projects`;
- nested children render with server-side `<details>` and stable anchors without JavaScript;
- raw content remains escaped; and
- existing CSP, CSRF, ownership, upload, and revision tests continue to pass.

### Compatibility

- schema-v1 packages read as one-main-source families;
- existing one-source uploads remain valid;
- existing library, user, project, and hosted transcript URLs continue to resolve; and
- existing `serve` deployments retain catalog routes and library behavior; the local live source set intentionally becomes current-project scoped unless `--all-projects` is used.

## Reference Use

The pinned `references/claude-code-transcripts` and `references/sniffly` submodules are design and test references only. They are not runtime or build dependencies.

`claude-code-transcripts` informs the focused interactive picker and single-session HTML navigation. Its local finder recursively scans all projects and excludes agent files by default, so it is not evidence for project scoping or complete subagent composition.

Sniffly informs project-first log-directory resolution, provider-record preservation, streaming interaction deduplication, sidechain classification, and scalable message navigation. It is not the authority for parent-child attachment; local Claude `agentId` and tool-result records define that relationship.

## Non-Goals

- Analytics dashboards, token-cost reporting, and error taxonomies from Sniffly
- Claude web-session APIs or cloud credential extraction
- Automatic publication or sharing without an explicit import/upload action
- Collapsing sibling Git worktrees into one default project
- Arbitrary inference of child relationships from descriptions, timestamps, or text similarity
- A generic event-graph framework before another provider requires it
- Rewriting immutable schema-v1 packages in place

## Implementation Boundary

This design is one implementation-program scope but must be planned in dependency order:

1. project identity and provider fixture expansion;
2. provider normalization and stable Claude attachment;
3. family discovery and atomic snapshot eligibility;
4. schema-v2 family model, content identity, and stores;
5. import and hosted multipart family transport;
6. focused browse and all-project command behavior; and
7. review projection, nested rendering, compatibility, and end-to-end verification.

No presentation-only shortcut is acceptable before the parser, discovery, identity, and persistence contracts are in place.
