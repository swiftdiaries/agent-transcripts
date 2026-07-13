# Agent Transcripts

Browse and publish completed Claude Code and Codex sessions through one small Go
binary. It retains the original JSONL plus a normalized, server-rendered view.

## Install and browse locally

Install a Go toolchain compatible with this repository, then build the binary:

```sh
go install github.com/swiftdiaries/agent-transcripts/cmd/agent-transcripts@latest
agent-transcripts version
agent-transcripts serve --open
```

Local mode listens on `127.0.0.1:8080` and discovers sessions from
`~/.claude/projects`, `~/.codex/sessions`, and `~/.codex/archived_sessions`.
Visit the displayed local URL to browse live and library pages. Use
`agent-transcripts serve --config config.example.yaml` to customize local roots,
quiet period, or the filesystem library root.

Import creates an immutable library package only after completion is revalidated:

```sh
agent-transcripts import
agent-transcripts import --latest
agent-transcripts import /path/to/completed-session.jsonl
```

An active or incomplete session is rejected. A previously written local file is
accepted only when its descriptor is revalidated as completed; importing a path
never bypasses eligibility.

## Publish to a hosted library

First import the completed session and use its returned package ID. Then publish
it to a user or project directory. The token is read from the environment (or
prompted from an interactive terminal) and is never written to YAML.

```sh
# Set AGENT_TRANSCRIPTS_TOKEN through your shell or secret manager first.
agent-transcripts upload \
  --server https://transcripts.example.com \
  --destination projects/platform \
  --title "Parser design" \
  --tags "go,parsers" \
  <library-package-id>
```

The command shows a confirmation prompt; pass `--yes` only after reviewing the
publication summary. Repeating the same immutable package to the same destination
returns the existing stable URL. Publishing it to another destination creates a
distinct URL.

## Host the server

Copy `config.example.yaml` and select one of its hosted examples. Hosted mode
requires HTTPS in `external_origin`, an authentication mode, cookie-signing keys,
and a bearer-token signing key. Supply secret values only through the environment:

```sh
# Ensure AGENT_TRANSCRIPTS_COOKIE_KEY_CURRENT and AGENT_TRANSCRIPTS_TOKEN_KEY
# are set by the deployment's secret manager before starting the service.
agent-transcripts serve --config /etc/agent-transcripts/config.yaml
```

For filesystem storage, mount a writable library directory. For S3-compatible
storage, configure the bucket, prefix, region, and optional endpoint in YAML, then
provide storage credentials through the platform's standard workload identity or
environment credential chain—not through YAML. The current v1 `serve` composition
uses filesystem storage; retain the S3 example as the deployment contract while
S3 serving is completed.

Terminate TLS at the reverse proxy and set `external_origin` to its public HTTPS
origin. In proxy-auth mode, make the application network-reachable only from the
listed `trusted_proxy_cidrs`; otherwise an untrusted peer could forge identity
headers. In OIDC mode, set the issuer, client ID, secret environment-variable name,
and HTTPS callback URL. Do not expose hosted mode directly to the internet.

Cookie keys support rotation: put the new 32-byte-or-longer key first in
`cookie_key_envs`, retain the previous key while existing sessions expire, then
remove it. Rotate `token_key_env` separately; it invalidates existing bearer tokens.

### Container

The Docker image is a static Linux binary in a digest-pinned, non-root distroless
runtime. Filesystem mode is the only mode requiring a writable application volume:

```sh
docker build -t agent-transcripts:test .
docker run --rm -p 127.0.0.1:8080:8080 \
  -v "$PWD/agent-transcripts-library:/var/lib/agent-transcripts" \
  -v "$PWD/config.yaml:/etc/agent-transcripts/config.yaml:ro" \
  agent-transcripts:test serve --config /etc/agent-transcripts/config.yaml
docker image inspect agent-transcripts:test --format '{{.Config.User}}'
```

The final command should report the non-root runtime user.

## Claude and Skills installations

Install this repository as a Claude marketplace, then install its plugin:

```sh
claude plugin marketplace add .
claude plugin install agent-transcripts@agent-transcripts
```

Or install the portable skill directly with `npx skills`:

```sh
npx skills add https://github.com/swiftdiaries/agent-transcripts --skill publish-transcript
```

Validate the distribution layout before publishing changes:

```sh
python -m json.tool .claude-plugin/plugin.json
python -m json.tool .claude-plugin/marketplace.json
claude plugin validate .
```

The skill checks for the installed binary, rejects the currently active session,
imports a completed session, shows a publication summary, requires confirmation,
and invokes `agent-transcripts upload`. Installing it never authorizes background
uploads. Raw browser uploads without terminal import evidence are rejected by the
skill; do not use a direct path or browser upload to bypass completion checks.

## v1 privacy and completion limits

V1 handles only completed Claude Code and Codex JSONL sessions. Completion is an
operational check based on provider terminal evidence when available, otherwise an
unchanged quiet period and exclusion of the active session; it does not guarantee a
provider will never resume a source later. The original source and normalized
transcript contain agent content, so operate hosted deployments inside a trusted
company boundary. There are no public share links, background synchronization,
full-text search, retention policy, or project ACLs in v1.

Logs are metadata-only: they must not contain transcript content, raw source, bearer
tokens, cookie keys, OIDC client secrets, or storage credentials.
