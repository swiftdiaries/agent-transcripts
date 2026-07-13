# README Usage Section Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give first-time users a concise, accurate command path for browsing, importing, and publishing transcripts.

**Architecture:** Add one task-oriented Markdown section immediately below the README introduction. It summarizes the existing CLI workflow and links to the established publishing guide instead of duplicating deployment, configuration, or privacy documentation.

**Tech Stack:** Markdown; the existing Go CLI command contract in `internal/cli/commands.go`.

## Global Constraints

- Modify `README.md` only for the implementation; do not change CLI or configuration behavior.
- Document only implemented command names and arguments.
- State that imports require completed sessions and that hosted publishing uses a completed library package and bearer token.
- Do not include secret values or advise bypassing completion checks.

---

### Task 1: Add the task-oriented usage guide

**Files:**
- Modify: `README.md:6` (insert a `## Usage` section before `## Install and browse locally`)
- Test: `internal/cli/commands.go:60-75,95-109,143-170` (read-only command-contract check)

**Interfaces:**
- Consumes: The CLI commands `serve --open`, `import`, `import --latest`, `import <completed-jsonl-path>`, and `upload --server <https-url> --destination <path> <library-package-id>`.
- Produces: A concise top-level README workflow whose commands are runnable with the documented CLI.

- [x] **Step 1: Add the Usage section after the introductory paragraph**

  Add this Markdown before `## Install and browse locally`:

  ```markdown
  ## Usage

  The usual workflow is **browse → import → publish**.

  1. Browse completed local sessions and the library:

     ```sh
     agent-transcripts serve --open
     ```

  2. Import a completed session interactively, select the newest, or provide its
     JSONL path:

     ```sh
     agent-transcripts import
     agent-transcripts import --latest
     agent-transcripts import /path/to/completed-session.jsonl
     ```

     Active or incomplete sessions are rejected.

  3. Publish an imported package to a hosted user or project directory:

     ```sh
     agent-transcripts upload \\
       --server https://transcripts.example.com \\
       --destination projects/platform \\
       <library-package-id>
     ```

     `upload` asks for confirmation unless `--yes` is supplied and reads its
     short-lived bearer from `AGENT_TRANSCRIPTS_TOKEN` or an interactive prompt.
     See [Publish to a hosted library](#publish-to-a-hosted-library) for metadata,
     token, and idempotency details.
  ```

- [x] **Step 2: Verify Markdown and the documented command contract**

  Run:

  ```sh
  git diff --check
  rg -n 'serve --open|import --latest|upload requires --server, --destination' README.md internal/cli/commands.go
  ```

  Expected: `git diff --check` produces no output; `rg` finds the documented
  local commands and the upload implementation's required flags.

- [x] **Step 3: Commit the documentation change**

  Run:

  ```sh
  git add README.md
  git commit -m "docs: add README usage section"
  ```
