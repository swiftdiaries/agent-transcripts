# Evidence Ledger Frontend Design

## Goal

Replace the browser UI’s unstyled presentation with a minimal, navigable
evidence-ledger interface for browsing, reading, importing, and publishing
agent transcripts. The application remains server-rendered and preserves every
existing route, form name, HTTP method, API endpoint, and security control.

## Product Context

Agent Transcripts is a developer-facing archive for completed Claude Code and
Codex sessions. Its audience needs to distinguish source, status, destination,
and event type quickly while retaining comfortable long-form reading. The
referenced `claude-code-transcripts` export demonstrates the useful part of a
transcript-specific visual language: provenance and event state are visible at
a glance. This design keeps that clarity without carrying forward its
card-heavy, older component style.

## Visual Direction: Evidence Ledger

The interface should resemble a compact engineering record rather than a
dashboard or generic document uploader. The one memorable device is a
**receipt strip**: a short monospace line for source, state, timestamps,
provider, event kind, or destination. It appears before record titles and
transcript events, making provenance part of the scan path.

### Tokens

| Purpose | Value | Use |
| --- | --- | --- |
| Ink | `#16324F` | Masthead, primary action, key headings |
| Paper | `#F7F8F6` | Application background |
| Surface | `#FFFFFF` | Forms and record panels |
| Ledger gray-blue | `#5E7683` | Receipt strips and secondary metadata |
| Rule | `#D7DFE2` | Borders and dividers |
| Verified teal | `#0F766E` | Completed / user-event accent |
| Amber | `#B45309` | Warning or pending state only |

Typography uses Georgia for page theses and transcript titles, the platform
system sans-serif for body copy and controls, and `ui-monospace` for metadata.
No remote font dependency is introduced.

## Shared Shell

- `layout.html` becomes a structured masthead and main content shell.
- The masthead preserves the existing Home, Live, Library, and Upload links,
  gives the product name an uppercase monospace treatment, and renders Upload
  as the compact primary navigation action.
- `app.css` owns all visual tokens, responsive behavior, focus states, native
  control styling, and `prefers-reduced-motion` handling.
- `app.js` retains copy-link behavior and may add only progressive form affordances
  that work without altering submission semantics; JavaScript is not required
  to navigate or submit any page.

## View Designs

### Home

Keep the existing local/live and imported-library destinations. Present a
short editorial thesis, two destination cards, and an Upload action. The home
page must give a first-time user an obvious next route without inventing a
dashboard or metrics.

### Live and Library directories

Keep the existing forms and links. Render each candidate or saved transcript as
a compact record with a receipt strip and a single clickable title. The Live
import form keeps its checkbox names and POST destination; its completion
status remains visible beside the transcript rather than becoming decorative.
Empty states direct users to the valid next action.

### Transcript reader

Keep prompt-anchor navigation and copy-link buttons. Use a constrained reading
column, a sticky-on-wide-screens prompt index when practical with CSS, receipt
strips for event kind and ordinal, and event-kind rails that distinguish user
events from assistant events without depending on color alone. Tool inputs,
outputs, raw events, and long text remain inside native `details` and `pre`
elements so transcript fidelity is unchanged.

### Upload

Keep the multipart form, hidden CSRF field, field names, required flags, and
POST target exactly as implemented. Add a descriptive thesis, a source-file
first layout, grouped optional metadata, a concise completion safeguard, and a
clear `Publish transcript` action. The two-column desktop form collapses to one
column at small viewport widths.

## Interaction and Accessibility

- Use only short hover/focus transitions; do not add decorative animation.
- Honor `prefers-reduced-motion: reduce`.
- Preserve native file input and form submission behavior.
- Provide visible `:focus-visible` treatment and maintain WCAG-friendly text
  contrast against Paper and Surface.
- Keep all labels programmatically associated with their controls.
- Do not introduce icons that are required to understand navigation or state.

## Files and Scope

- Modify `internal/web/templates/layout.html` for the shared shell.
- Modify `internal/web/templates/home.html`, `directory.html`,
  `transcript.html`, and `upload.html` for semantic structure and copy only.
- Replace `internal/web/static/app.css` with the responsive token-based design.
- Modify `internal/web/static/app.js` only if a progressive enhancement supports
  the established interaction model; otherwise leave it unchanged.
- Extend `internal/web/server_test.go` only for stable structural or behavioral
  assertions that are affected by the template changes.
- Do not add a frontend framework, external font, dependency, route, endpoint,
  configuration field, or browser-side upload implementation.

## Verification

- Run `gofmt` on changed Go test files, if any.
- Run `go test -race -count=1 ./...`, `go vet ./...`, and build the CLI.
- Inspect the served pages through existing handler tests and a local browser
  preview when loopback binding is available.
- Run `git diff --check`.
