# Project session browsing and usage

Run `agent-transcripts` from inside a Git worktree to open a loopback-only
dashboard for the current project. It discovers completed Claude Code and
Codex session families whose recorded working directory belongs to that
project. The dashboard defaults to the most recent `7d` of transcript usage;
it does not publish a server or contact a provider.

Use `agent-transcripts --global` to open the explicit all-projects dashboard.
It lists projects first and then opens a project-scoped session list. Global
mode is an opt-in view: the normal command remains limited to the current
project.

Focused transcript browsing remains available when that is the task at hand:

```sh
agent-transcripts browse --latest
agent-transcripts browse --family <opaque-family-key>
agent-transcripts browse /path/to/completed-session.jsonl
```

`browse` without a selector provides an interactive chooser. In a
non-interactive shell, use `--latest`, `--family`, or a path; add `--no-open`
when you do not want the browser launched. A focused browse exposes only the
selected family, not the live project catalog.

## Time ranges and usage

The dashboard's ranges use UTC calendar days. `7d`, `30d`, and `90d` include
today plus the preceding 6, 29, or 89 UTC days respectively. `all` includes
every eligible usage sample. A custom range requires both `from` and `to` as
`YYYY-MM-DD`; it includes the whole UTC day named by each endpoint (the
effective end is the next UTC midnight after `to`).

Usage is parser-normalized evidence from transcript assistant messages. Token
and cost totals include a family main session and its attached delegated
children. A session workspace opens on **Overview**, with a delegation map,
per-agent model/token/cost rows, and links to the main and delegated streams.
**All activity** is the only view with an author filter:

- **User** means user-authored transcript events from any stream.
- **Main agent** means assistant/tool events in the root session.
- **Agent `<id>`** means assistant/tool events in that delegated agent's
  stream.

Selecting an author changes only All activity. It does not change the
Overview totals, the main stream, or a delegated stream.

## Pricing and offline operation

The dashboard loads a local pricing catalog and displays its source, whether
it is stale, and any tokens whose model/rate is unpriced. Refresh deliberately
is the only dashboard-related command that makes a network request:

```sh
agent-transcripts pricing refresh
```

It fetches the LiteLLM model-price catalog, retains supported Anthropic and
OpenAI model rates, and writes the local catalog. Ordinary project/global
dashboards and focused workspaces work offline; they use the cached catalog,
or the embedded snapshot when no valid local catalog is available. A catalog
older than 24 hours is labelled stale. Unknown models or missing rate
components remain visible as unpriced tokens rather than being silently
estimated.

Estimated API-equivalent cost is not subscription spend. Subscription quota is not available from transcript data.

## Parsed-family cache

The application uses `os.UserCacheDir()` and stores its private cache under
the resolved `agent-transcripts` directory:

- `families-v1` holds normalized parsed session families;
- `pricing.json` holds the last pricing refresh.

The cache directory is created with owner-only permissions (`0700`) and its
family entries with owner-only permissions (`0600`). In memory it retains at
most 32 families. On disk it retains at most 256 entries and 512 MiB, evicting
the oldest entries as needed.

A parsed family is reused only while its opaque revision key still matches the
provider, project, parser-normalization version, and source file identities.
Changing a source file or the normalization version therefore causes a fresh
parse. Invalid, malformed, or incompatible cache entries are ignored and
rebuilt.

To clear cached data safely, first resolve the platform-specific path returned
by `os.UserCacheDir()`. Remove only the `agent-transcripts/families-v1`
directory and/or the `agent-transcripts/pricing.json` file inside that resolved
cache directory. Do not use a recursive deletion against a home directory or
an unresolved environment variable.
