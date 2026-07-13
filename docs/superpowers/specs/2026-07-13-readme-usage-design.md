# README Usage Section Design

## Goal

Add a short, task-oriented `Usage` section near the top of the README so a
first-time user can see the three primary workflows without reading the full
hosting guide.

## Scope

The section will contain three ordered flows:

1. Browse locally with `serve --open`.
2. Import a completed transcript with interactive, latest, and explicit-path
   examples.
3. Publish an already-imported package with its destination and bearer-token
   requirements.

It will link readers to the existing detailed sections for hosted deployment,
container operation, plugins, and privacy limits rather than duplicating them.

## Constraints

- Document only implemented CLI behavior.
- Keep completion safeguards explicit: imports reject active/incomplete files;
  hosted publishing requires a bearer token and a completed library package.
- Never include token values or recommend bypassing completion checks.
- Modify `README.md` only; no CLI or configuration behavior changes.

## Verification

- Review the rendered Markdown structure and command names against
  `internal/cli/commands.go`.
- Run `git diff --check`.
