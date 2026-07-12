# Agent Transcripts v1 Design

## Summary

Agent Transcripts is a minimal Go application for viewing and sharing completed
Claude Code and Codex sessions through one consistent HTML interface. The same
binary works locally, on a VM, or in Kubernetes. Local mode can browse agent
session files directly or import immutable copies into a managed library.
Hosted mode lets authenticated employees upload sessions into openly browsable
user and project directories, similar to Commuter.

The first release optimizes for a complete, understandable vertical slice. It
does not attempt to become a search engine, collaboration suite, or Kubernetes
platform.

## Goals

- Render completed Claude Code and Codex sessions in one HTML viewer.
- Browse local session directories without copying files.
- Import selected sessions into a managed local library.
- Upload through a browser, CLI, or explicitly invoked agent skill.
- Browse hosted transcripts by user or project directory.
- Run as one Go binary with filesystem or S3-compatible storage.
- Support company identity through trusted proxy headers or OIDC.
- Preserve both the original transcript and a canonical normalized form.

## Non-goals

The following are outside v1:

- Live or background session synchronization.
- Uploading active or incomplete sessions.
- Public share links or unauthenticated access.
- Project-level ACLs.
- Full-text search, metadata filtering, or tag navigation.
- Comments, reactions, annotations, favorites, or dashboards.
- A SPA or separately deployed frontend.
- A database, queue, cache, event bus, or runtime plugin system.
- Helm charts, Kubernetes manifests, metrics, tracing, backups, retention
  policies, or high-availability coordination.

Directory-local tag filtering is a candidate for v2.

## System Shape

One `agent-transcripts` Go binary provides:

```text
agent-transcripts serve
agent-transcripts import [path]
agent-transcripts upload [library-session-id]
agent-transcripts version
```

`serve` starts the same server-rendered HTML application in local or hosted
mode. HTML templates, CSS, and minimal progressive JavaScript are embedded in
the binary.

Only three volatile boundaries use Go interfaces:

- `Parser`: Claude or Codex JSONL to a canonical session.
- `Store`: filesystem or S3-compatible object storage.
- `Identity`: local implicit user, trusted proxy headers, or OIDC.

HTTP handlers call application functions directly. Application functions call
these interfaces directly. There is no internal plugin framework or messaging
layer.

## Modes

### Local mode

Local mode exposes two sources through one UI:

- **Live sessions** are discovered directly from configured Claude and Codex
  directories. Opening one parses and renders it without copying it.
- **Library sessions** are completed sessions explicitly imported as immutable
  snapshots into managed filesystem storage.

A live session may still change on disk. It therefore has no managed permalink
and cannot be uploaded until it is complete and imported. An imported session
has stable content and a library session ID.

### Hosted mode

Hosted mode runs inside a company VPC and requires company authentication. All
authenticated employees may browse all user and project directories. Directory
structure organizes knowledge; it is not an authorization boundary.

Users may upload into their own user directory or any project directory. A
project is created when an authenticated user first uploads to a new valid
project slug.

## Discovering and Choosing Sessions

Default discovery roots are:

```text
~/.claude/projects/**
~/.codex/sessions/**
~/.codex/archived_sessions/**
```

YAML configuration or CLI flags may override them. Each provider adapter does a
cheap metadata pass and returns the provider, project or repository, start
time, first prompt or title, status, and provider session ID. Results from all
providers are merged and sorted newest first. Setup-only, malformed, and active
sessions are hidden by default.

For v1, "completed" is an operational eligibility check rather than a promise
that a provider will never resume the session. A provider adapter treats a
session as completed when it has provider-specific terminal evidence, when
available. Otherwise, the source file must be unchanged for a configurable
quiet period (five minutes by default) and must not be identifiable as the
currently active session. Import creates an immutable snapshot; later resuming
the provider session does not mutate that snapshot.

Running `agent-transcripts import` without a path opens a searchable,
multi-select terminal picker. Filtering matches displayed metadata only; it is
not transcript-content search.

Example non-interactive usage:

```bash
agent-transcripts import --latest
agent-transcripts import --provider claude --limit 20
agent-transcripts import /path/to/session.jsonl
agent-transcripts upload <library-session-id> --destination projects/platform
```

The local HTML UI lists merged live sessions and library sessions separately.
Users may open a live session directly or select completed live sessions to
import into the library. Library sessions expose upload actions.

## Canonical Session Package

Each managed session is one immutable package:

```text
sessions/<canonical-session-id>/
|-- metadata.json
|-- normalized.json
`-- source.jsonl
```

`source.jsonl` is the unmodified original. It is stored but has no download
link in v1.

`normalized.json` is a versioned, ordered canonical event document. Common
event types include:

- User and assistant messages.
- Tool calls and results.
- File changes and commits.
- Errors.
- Nested-agent relationships.
- Provider events not yet understood by the canonical model, represented as
  `raw` events with their original type and payload.

The canonical model does not invent equivalence between provider-specific
semantics. Unknown event types are preserved rather than treated as errors.

`metadata.json` contains:

- Canonical session ID.
- Provider and provider session ID.
- Mutable title, description, and tags.
- Original project or repository and a display-safe working-directory label.
- Session start and end timestamps.
- Uploader identity and upload timestamp.
- Logical destination directory.
- Source checksum.
- Parser and normalized-schema versions.

Tags are optional free-form labels such as `frontend`, `rust`, or
`project-1123`. They are trimmed, lowercased, deduplicated, and restricted to
ASCII letters, digits, hyphens, and underscores. V1 displays tags but does not
use them for filtering or navigation.

## Import and Upload Flows

### Local import

```text
discover file
-> verify completed session
-> identify provider
-> checksum source
-> parse and normalize
-> validate normalized document
-> write immutable package
-> return library session ID
```

The source checksum makes repeated local imports idempotent. Re-importing the
same source returns the existing library entry.

### Hosted upload

```text
choose library session and destination
-> send raw source plus catalog metadata
-> authenticate user
-> detect provider and checksum source
-> parse, normalize, and validate on the server
-> store the package
-> return the stable transcript URL
```

The server does not trust client-generated normalized content. It always
normalizes the raw source with its installed parser version. Browser file
uploads enter at the same server-side parsing step.

Uploading the same source checksum to the same destination returns the existing
session URL. The same source may be published separately to a different
destination.

Transcript content, source, provider identity, checksum, and provenance are
immutable. The original uploader may edit title, description, and tags; move
the transcript; or delete it.

## Directories and Storage

The hosted catalog has two logical namespaces:

```text
/users/<sso-username>/
/projects/<project-slug>/
```

A user directory is created on that user's first upload. Any authenticated
employee may browse both namespaces and all transcripts within them.

The storage boundary exposes domain operations rather than filesystem or S3
primitives:

```go
ListDirectories(kind)
CreateProject(slug)
ListSessions(directory)
GetSession(id)
PutSession(package)
UpdateMetadata(id, metadata)
MoveSession(id, destination)
DeleteSession(id)
```

The filesystem implementation uses ordinary directories and files. The S3
implementation maps the logical tree to object prefixes. Publication writes
the package data first and a final manifest last. Listings show only packages
with a completed manifest, so interrupted writes do not expose partial
transcripts.

## Authentication and Authorization

Configuration selects exactly one identity mode:

- `local`: one implicit user for local use.
- `proxy`: trust configured identity headers added by an authenticated reverse
  proxy.
- `oidc`: use the OIDC authorization-code flow and a signed application
  session.

Hosted upload and mutation routes require `proxy` or `oidc` authentication. The
application derives a stable internal user key and display name from the
authenticated identity. Upload payloads cannot specify their uploader.

Every authenticated employee may:

- Browse all users, projects, and transcripts.
- Create a project by publishing to a new project slug.
- Upload to their own user directory or any project directory.

Only the original uploader may edit metadata, move, or delete a transcript.

## Configuration

The binary accepts a YAML file through `--config`. CLI flags override YAML
values. Credentials and secrets come from environment variables or workload
identity rather than YAML.

Example hosted configuration:

```yaml
mode: hosted

storage:
  type: s3
  bucket: agent-transcripts
  prefix: production/

auth:
  type: proxy
  proxy:
    user_header: X-Forwarded-User
    name_header: X-Forwarded-Name
```

Configuration validates at startup and fails with a concise error when a
required value is absent or incompatible with the selected mode.

## HTML Interface

The frontend has four primary pages:

1. **Home** links to Users, Projects, Local Live Sessions, and Local Library as
   applicable to the running mode.
2. **Directory listing** shows transcript cards newest first, including title,
   provider, uploader, date, repository or project, description, and tags.
3. **Transcript viewer** shows ordered prompts and responses. Tool calls and
   results are collapsed by default. Raw provider events are expandable. Each
   message has an anchored URL, and prompts have previous/next navigation.
4. **Upload/import form** accepts files and editable title, description, tags,
   and destination fields.

Core navigation and reading work without JavaScript. Embedded JavaScript is
limited to expanding blocks, copying anchored links, and improving multi-file
selection.

## CLI and HTTP API

CLI flags include `--config`, `--listen`, `--open`, `--latest`, `--provider`,
`--destination`, `--title`, `--description`, and repeatable `--tag`. Missing
values may be prompted for only in an interactive terminal. Non-interactive
commands fail with an actionable error.

The minimal mutation API is:

```text
GET    /api/v1/directories
POST   /api/v1/projects
POST   /api/v1/sessions
PATCH  /api/v1/sessions/{id}/metadata
POST   /api/v1/sessions/{id}/move
DELETE /api/v1/sessions/{id}
```

The server-rendered frontend uses normal HTML read routes. V1 does not duplicate
all read behavior as a JSON API. Uploads use multipart form data containing the
raw JSONL file and catalog metadata.

## Agent Plugin and Skill

The repository follows the plugin layout used by `hamelsmu/evals-skills`:

```text
.claude-plugin/
|-- plugin.json
`-- marketplace.json
skills/
`-- publish-transcript/
    `-- SKILL.md
```

The Claude plugin manifest makes the repository installable through a plugin
marketplace. The root `skills/` directory also supports portable skill
installers such as `npx skills add`.

`publish-transcript/SKILL.md` tells a compatible agent to:

1. Confirm the requested session is completed.
2. Resolve the selected local library session, importing it when necessary.
3. Collect the destination and optional title, description, and tags.
4. Display a concise publication summary.
5. Invoke `agent-transcripts upload`.
6. Return the hosted transcript URL.

The skill checks that the Go binary is installed and configured. It contains no
parser or HTTP client logic and does not need a shell wrapper. Installing the
skill never enables automatic collection; it acts only after an explicit
request.

Because v1 accepts completed sessions only, a skill invoked inside the current
active session cannot publish that session. It can publish a selected or most
recent completed session and must explain the constraint if asked to publish
the active one.

## Error Behavior

Import or upload stops when the provider cannot be identified, the session is
active or incomplete, JSONL is malformed, normalized validation fails,
metadata or destination is invalid, authentication is missing, or storage
cannot finalize the package.

Errors are concise and actionable. Logs never contain raw transcript bodies,
tool output, authentication tokens, OIDC secrets, or storage credentials.

## V1 Verification

The critical test suite covers:

- One representative sanitized Claude fixture.
- One representative sanitized Codex fixture.
- Unknown-event preservation.
- Rejection of incomplete sessions.
- Filesystem import and read.
- S3-compatible import and read using a test double.
- Authentication and uploader-only mutations.
- Safe HTML escaping and rendering.
- One end-to-end import, upload, and browse flow.

## Deployment Artifacts

V1 produces:

- One Go binary with embedded HTML assets.
- One minimal container image.
- Example YAML configuration.
- A simple `/healthz` endpoint.

The same binary and configuration model are used for local, VM, and Kubernetes
deployments.

## References

- [simonw/claude-code-transcripts](https://github.com/simonw/claude-code-transcripts)
  for Claude session discovery and transcript rendering behavior.
- [prateek/codex-transcripts](https://github.com/prateek/codex-transcripts)
  for Codex rollout discovery and normalized transcript precedent.
- [nteract/commuter](https://github.com/nteract/commuter) for directory-oriented
  browsing and hosted sharing.
- [hamelsmu/evals-skills](https://github.com/hamelsmu/evals-skills) for the
  repository-level Claude plugin and portable skill layout.
