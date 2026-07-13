---
name: publish-transcript
description: Import and publish an eligible completed Claude Code or Codex session to an Agent Transcripts hosted library.
---

# Publish a transcript

Use this skill only for an explicitly requested, foreground publication. Installation
never authorizes background uploads, scheduled synchronization, or publication of
any session without the user's confirmation.

1. Verify the CLI is installed before inspecting or publishing anything:

   ```sh
   command -v agent-transcripts
   agent-transcripts version
   ```

   If it is unavailable, stop and ask the user to install the binary. Do not use a
   raw browser upload or another upload client as a substitute.

2. Determine what the user wants to publish. The currently active Claude Code or
   Codex session is ineligible and must be rejected: explain that v1 only publishes
   completed sessions, then offer to select another completed session. Never suggest
   bypassing this rule with a direct source path or a raw browser upload.

3. Resolve a completed session through the CLI. Prefer the requested completed
   session; otherwise list eligible choices interactively, or use the user's explicit
   provider/recency instruction:

   ```sh
   agent-transcripts import
   # or, only when the user explicitly chooses the newest eligible session:
   agent-transcripts import --latest
   ```

   Record the returned immutable library package ID. `import` revalidates completion
   evidence and rejects active or incomplete sources; do not claim a session is
   publishable until it returns an ID.

4. Collect the hosted server URL, required destination (`users/<slug>` or
   `projects/<slug>`), and optional title, description, and comma-separated tags.
   The bearer token must be supplied by the user through `AGENT_TRANSCRIPTS_TOKEN`
   or an interactive terminal prompt. Do not request, echo, retain, or place a token
   in chat, a command transcript, or metadata.

5. Before publishing, show this exact summary with the gathered values (use
   `(none)` for omitted optional fields), then ask for a clear yes/no confirmation:

   ```text
   Publication summary
   Package: <library-package-id>
   Server: <https://transcripts.example.com>
   Destination: <users/slug or projects/slug>
   Title: <title or (none)>
   Description: <description or (none)>
   Tags: <tags or (none)>
   ```

6. After confirmation, invoke the CLI—not a browser upload—with the selected
   options. `--yes` is appropriate only after the confirmation above:

   ```sh
   agent-transcripts upload \
     --server "<server>" \
     --destination "<destination>" \
     --title "<title>" \
     --description "<description>" \
     --tags "<tag1,tag2>" \
     --yes \
     "<library-package-id>"
   ```

   Omit any optional flag whose value is `(none)`. Return only the stable URL
   printed by the command. A repeated upload of the same immutable package to the
   same destination returns its existing URL.

Raw browser uploads without terminal import evidence are rejected for this skill.
Never use a direct file path to evade session eligibility, and never publish in the
background.
