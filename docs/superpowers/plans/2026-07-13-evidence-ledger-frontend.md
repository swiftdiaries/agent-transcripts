# Evidence Ledger Frontend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver a minimal, responsive Evidence Ledger interface across Home, Live, Library, transcript reader, and Upload without changing application behavior.

**Architecture:** Keep the Go HTTP server and embedded static assets unchanged in shape. Add a small `Section` presentation field to the existing `page` view model so the shared layout can mark the current route; replace the one-line templates with semantic markup and apply the complete visual system in `app.css`. Preserve progressive enhancement: every navigation and form flow works without JavaScript.

**Tech Stack:** Go `html/template`, embedded CSS and JavaScript assets, existing `net/http` handler tests, standard browser CSS.

## Global Constraints

- Preserve every existing route, form name, HTTP method, API endpoint, CSRF field, multipart field, required flag, and server-side security control.
- Do not add a frontend framework, external font, dependency, route, endpoint, configuration field, or browser-side upload implementation.
- Use Ink `#16324F`, Paper `#F7F8F6`, Surface `#FFFFFF`, Ledger gray-blue `#5E7683`, Rule `#D7DFE2`, Verified teal `#0F766E`, and Amber `#B45309` only for warning or pending state.
- Use Georgia for editorial titles, the system sans-serif for body and controls, and `ui-monospace` for receipt-strip metadata. Do not fetch remote fonts.
- Preserve native file input, labels, submit behavior, `details`, `pre`, prompt anchors, and copy-link controls.
- Provide visible `:focus-visible` styles, reduced-motion support, and a single-column mobile layout.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/web/handlers.go` | Supply a stable `Section` string to page templates without changing route behavior. |
| `internal/web/templates/layout.html` | Shared masthead, landmarks, route-aware navigation, and static asset inclusion. |
| `internal/web/templates/home.html` | First-time-user thesis and destination choices. |
| `internal/web/templates/directory.html` | Live import catalog and stored-transcript record index. |
| `internal/web/templates/transcript.html` | Prompt index plus readable, source-aware event stream. |
| `internal/web/templates/upload.html` | Guided multipart upload form with unchanged controls. |
| `internal/web/static/app.css` | Complete tokenized Evidence Ledger visual system and responsive rules. |
| `internal/web/static/app.js` | Existing copy-link progressive enhancement only. |
| `internal/web/server_test.go` | Structural regression tests for navigation, form contract, prompt anchors, and static assets. |

### Task 1: Add route-aware presentation data and regression checks

**Files:**
- Modify: `internal/web/handlers.go:510-549`
- Modify: `internal/web/server_test.go:398-431`

**Interfaces:**
- Consumes: `page` values produced by `home`, `liveList`, `library`, `directory`, `liveSession`, `transcript`, and the `/upload` route.
- Produces: `page.Section string` with exactly `home`, `live`, `library`, `transcript`, or `upload`; the shared layout consumes it to set the current navigation item.

- [ ] **Step 1: Write the failing presentation-model regression test**

  Add this test after `TestCorePagesWorkWithoutJavaScript` in
  `internal/web/server_test.go`:

  ```go
  func TestTranscriptPageUsesTranscriptSection(t *testing.T) {
      got := transcriptPage(fixturePackage(t).Session, "Example transcript")
      if got.Section != "transcript" {
          t.Fatalf("section = %q, want transcript", got.Section)
      }
  }
  ```

- [ ] **Step 2: Run the focused test to verify it fails**

  Run:

  ```sh
  GOCACHE=$(pwd)/.go-cache go test ./internal/web -run TestTranscriptPageUsesTranscriptSection -count=1
  ```

  Expected: FAIL because `transcriptPage` currently returns an empty `Section`.

- [ ] **Step 3: Add the `Section` field and assign it at each page boundary**

  Extend the view model in `internal/web/handlers.go`:

  ```go
  type page struct {
      Title      string
      Heading    string
      Section    string
      Sessions   []session.Metadata
      Candidates []discovery.Candidate
      IsLive     bool
      Transcript transcript
      CSRFToken  string
  }
  ```

  Set the field in the existing render inputs without changing their other
  values:

  ```go
  // home route
  page{Title: "Agent transcripts", Section: "home"}

  // live list
  page{Title: "Live sessions", Heading: "Live sessions", Section: "live", Candidates: candidates, IsLive: true}

  // library and directory listings
  page{Title: "Library", Heading: "Library", Section: "library", Sessions: all}
  page{Title: d.Kind + ": " + d.Slug, Heading: d.Kind + ": " + d.Slug, Section: "library", Sessions: items}
  ```

  Change `transcriptPage` to give transcript pages their own section:

  ```go
  p := page{Title: title, Section: "transcript", Transcript: transcript{Title: title}}
  ```

  In the existing `/upload` case in `route`, construct:

  ```go
  p := page{Title: "Upload", Section: "upload"}
  ```

- [ ] **Step 4: Run the focused test to verify the new data contract**

  Run:

  ```sh
  gofmt -w internal/web/handlers.go internal/web/server_test.go
  GOCACHE=$(pwd)/.go-cache go test ./internal/web -run 'Test(CorePagesWorkWithoutJavaScript|TranscriptPageUsesTranscriptSection)' -count=1
  ```

  Expected: both tests PASS; routes remain reachable and the transcript view
  model now identifies its presentation section.

- [ ] **Step 5: Commit the presentation-data foundation**

  ```sh
  git add internal/web/handlers.go internal/web/server_test.go
  git commit -m "feat: add frontend page sections"
  ```

### Task 2: Replace the shared shell and page templates with semantic Evidence Ledger markup

**Files:**
- Modify: `internal/web/templates/layout.html`
- Modify: `internal/web/templates/home.html`
- Modify: `internal/web/templates/directory.html`
- Modify: `internal/web/templates/transcript.html`
- Modify: `internal/web/templates/upload.html`
- Modify: `internal/web/server_test.go:398-431`

**Interfaces:**
- Consumes: `page.Section`, `page.Heading`, `page.Candidates`, `page.Sessions`,
  `page.Transcript`, and `page.CSRFToken` exactly as populated by handlers.
- Produces: Stable CSS hooks including `masthead`, `page-shell`,
  `receipt-strip`, `record-list`, `transcript-layout`, `event-card`, and
  `upload-form`; all existing URLs and HTML form field names remain unchanged.

- [ ] **Step 1: Write failing regression tests for page landmarks and interactive contracts**

  Add this test to `internal/web/server_test.go`:

  ```go
  func TestEvidenceLedgerTemplatesKeepInteractiveContracts(t *testing.T) {
      pkg := fixturePackage(t)
      h := newTestServer(t, pkg)
      checks := map[string][]string{
          "/upload": {
              `method="post" action="/api/v1/sessions" enctype="multipart/form-data"`,
              `name="csrf_token"`, `name="source"`, `name="destination"`,
              `name="title"`, `name="description"`, `name="tag"`,
              `class="upload-form"`,
          },
          "/sessions/" + pkg.ID: {
              `aria-label="Prompts"`, `class="transcript-layout"`,
              `class="copy-anchor"`,
          },
      }
      for path, tokens := range checks {
          t.Run(path, func(t *testing.T) {
              rr := httptest.NewRecorder()
              h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
              for _, token := range tokens {
                  if !strings.Contains(rr.Body.String(), token) {
                      t.Fatalf("%s missing from %s", token, path)
                  }
              }
          })
      }
  }

  func TestCorePagesRenderEvidenceLedgerLandmarks(t *testing.T) {
      h := newTestServer(t, fixturePackage(t))
      for path, want := range map[string]string{
          "/":        `data-section="home"`,
          "/library": `data-section="library"`,
          "/upload":  `data-section="upload"`,
      } {
          t.Run(path, func(t *testing.T) {
              rr := httptest.NewRecorder()
              h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
              body := rr.Body.String()
              for _, token := range []string{"<header class=\"masthead\"", "<main id=\"main-content\"", want} {
                  if !strings.Contains(body, token) {
                      t.Fatalf("%s missing from %s", token, path)
                  }
              }
          })
      }
  }
  ```

- [ ] **Step 2: Run the new template contract test and confirm the missing hooks**

  Run:

  ```sh
  GOCACHE=$(pwd)/.go-cache go test ./internal/web -run TestEvidenceLedgerTemplatesKeepInteractiveContracts -count=1
  ```

  Expected: FAIL because `masthead`, `upload-form`, and `transcript-layout` do
  not yet exist, while the existing field names remain present.

- [ ] **Step 3: Write the semantic templates with unchanged form and link contracts**

  Replace `layout.html` with this shared shell:

  ```html
  {{define "layout"}}<!doctype html>
  <html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width,initial-scale=1">
    <title>{{.Title}}</title>
    {{if .CSRFToken}}<meta name="csrf-token" content="{{.CSRFToken}}">{{end}}
    <link rel="stylesheet" href="/static/app.css">
  </head>
  <body data-section="{{.Section}}">
    <a class="skip-link" href="#main-content">Skip to content</a>
    <header class="masthead">
      <a class="wordmark" href="/">Agent transcripts</a>
      <nav class="primary-nav" aria-label="Primary navigation">
        <a href="/live" {{if eq .Section "live"}}aria-current="page"{{end}}>Live</a>
        <a href="/library" {{if eq .Section "library"}}aria-current="page"{{end}}>Library</a>
        <a class="nav-action" href="/upload" {{if eq .Section "upload"}}aria-current="page"{{end}}>Upload</a>
      </nav>
    </header>
    <main id="main-content" class="page-shell">{{template "content" .}}</main>
    <script src="/static/app.js" defer></script>
  </body>
  </html>{{end}}
  ```

  Use the following required content structure in the other templates:

  ```html
  <!-- home.html -->
  <section class="page-intro home-intro">
    <p class="eyebrow">A durable record of agent work</p>
    <h1>Keep the work,<br>not just the result.</h1>
    <p>Browse completed local sessions, preserve them in a shared library, and return to the decisions that made the work.</p>
  </section>
  <section class="route-grid" aria-label="Browse transcripts">
    <a class="route-card" href="/live"><span class="receipt-strip">01 / LOCAL SESSIONS</span><strong>Review completed work</strong><span>Live sessions ready to import</span></a>
    <a class="route-card" href="/library"><span class="receipt-strip">02 / SAVED LIBRARY</span><strong>Read the record</strong><span>Imported transcripts by person and project</span></a>
  </section>
  ```

  ```html
  <!-- directory.html: keep action, csrf_token, session value, and links exact -->
  <section class="page-intro"><p class="eyebrow">{{if .IsLive}}LOCAL CATALOG{{else}}TRANSCRIPT LIBRARY{{end}}</p><h1>{{.Heading}}</h1></section>
  {{if .IsLive}}<form class="selection-form" method="post" action="/live/import"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><p class="form-note">Choose completed sessions to add to the library.</p><ul class="record-list">{{range .Candidates}}<li class="record"><label><input type="checkbox" name="session" value="{{.Provider}}:{{.SessionID}}"><span><span class="receipt-strip">{{.Provider}} / {{.Status}}</span><a href="/live/{{.Provider}}/{{.SessionID}}">{{.Title}}</a><span class="record-detail">{{.Project}}</span></span></label></li>{{else}}<li class="empty-state">No completed sessions found. Start a session, then return once it is complete.</li>{{end}}</ul><button type="submit">Import selected sessions</button></form>{{else}}<ul class="record-list">{{range .Sessions}}<li class="record"><a href="/sessions/{{.ID}}"><span class="receipt-strip">{{.Provider}} / {{.Destination.Kind}}:{{.Destination.Slug}}</span><strong>{{.Title}}</strong><span class="record-detail">{{range .Tags}}{{.}} {{end}}</span></a></li>{{else}}<li class="empty-state">No transcripts found. Import a completed live session to begin the library.</li>{{end}}</ul>{{end}}
  ```

  ```html
  <!-- transcript.html: retain prompt anchor and all event payload paths -->
  <section class="page-intro transcript-intro"><p class="eyebrow">TRANSCRIPT / {{len .Transcript.Events}} EVENTS</p><h1>{{.Transcript.Title}}</h1></section>
  <div class="transcript-layout"><nav class="prompt-index" aria-label="Prompts"><p class="receipt-strip">PROMPT INDEX</p><ol>{{range .Transcript.Events}}{{if eq .Kind "user"}}<li><a href="#{{.ID}}">{{.Text}}</a></li>{{end}}{{end}}</ol></nav><section class="event-stream" aria-label="Transcript events">{{range .Transcript.Events}}<article id="{{.ID}}" class="event-card event-{{.Kind}}"><header><p class="receipt-strip">{{.Kind}} / {{.ID}}</p><button type="button" class="copy-anchor" data-anchor="{{.ID}}">Copy link</button></header>{{if .Text}}<pre>{{.Text}}</pre>{{end}}{{if .ToolName}}<details><summary>{{.ToolName}}</summary><pre>{{.Input}}</pre><pre>{{.Output}}</pre></details>{{end}}{{if .RawType}}<details><summary>{{.RawType}}</summary><pre>{{.Raw}}</pre></details>{{end}}</article>{{end}}</section></div>
  ```

  ```html
  <!-- upload.html: preserve method, action, enctype, field names, and required flags -->
  <section class="page-intro upload-intro"><p class="eyebrow">UPLOAD TO LIBRARY / 01</p><h1>Keep the work,<br>not just the result.</h1><p>Publish a completed session as a durable record for your team.</p></section>
  <form class="upload-form" method="post" action="/api/v1/sessions" enctype="multipart/form-data"><input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><div class="form-grid"><label class="file-field"><span class="receipt-strip">SOURCE FILE</span><input type="file" name="source" accept=".jsonl" required></label><label><span class="receipt-strip">DESTINATION</span><input name="destination" placeholder="projects/platform" required></label><label><span>Title <em>optional</em></span><input name="title"></label><label><span>Tag <em>optional</em></span><input name="tag"></label><label class="full-width"><span>Description <em>optional</em></span><textarea name="description"></textarea></label></div><div class="form-footer"><p>Only completed sessions can be published. Source facts are verified by the server.</p><button type="submit">Publish transcript</button></div></form>
  ```

- [ ] **Step 4: Run the complete web package after template replacement**

  Run:

  ```sh
  GOCACHE=$(pwd)/.go-cache go test ./internal/web -count=1
  ```

  Expected: PASS. This proves templates parse through `template.ParseFS`, every
  core page still renders without JavaScript, and the upload/transcript contract
  test finds the exact field names, endpoint, and controls.

- [ ] **Step 5: Commit the semantic template layer**

  ```sh
  git add internal/web/templates internal/web/server_test.go
  git commit -m "feat: add evidence ledger templates"
  ```

### Task 3: Implement the responsive tokenized visual system and verify assets

**Files:**
- Modify: `internal/web/static/app.css`
- Modify: `internal/web/static/app.js` only if required to retain copy-link feedback
- Modify: `internal/web/server_test.go:385-418`

**Interfaces:**
- Consumes: CSS hooks created in Task 2 and existing `button.copy-anchor[data-anchor]` behavior.
- Produces: A responsive style sheet that is served as `/static/app.css`; JavaScript stays optional and only copies a URL with the selected event fragment.

- [ ] **Step 1: Add a failing static-asset assertion for the design-system hooks**

  Extend `TestStaticAssetsHaveFixedContentTypeAndSecurityHeaders` with this
  separate test:

  ```go
  func TestEvidenceLedgerStylesExposeResponsiveAndAccessibleHooks(t *testing.T) {
      rr := httptest.NewRecorder()
      newTestServer(t, fixturePackage(t)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))
      for _, token := range []string{
          "--ink: #16324f", ".masthead", ".receipt-strip", ":focus-visible",
          "prefers-reduced-motion", "@media (max-width: 700px)",
      } {
          if !strings.Contains(strings.ToLower(rr.Body.String()), token) {
              t.Fatalf("stylesheet missing %q", token)
          }
      }
  }
  ```

- [ ] **Step 2: Run the static-asset test to verify it fails**

  Run:

  ```sh
  GOCACHE=$(pwd)/.go-cache go test ./internal/web -run TestEvidenceLedgerStylesExposeResponsiveAndAccessibleHooks -count=1
  ```

  Expected: FAIL because the current stylesheet has none of the Evidence Ledger
  tokens or responsive/accessibility selectors.

- [ ] **Step 3: Replace `app.css` with the complete visual-system contract**

  Implement the following required selector groups; values are intentionally
  explicit so all templates share one system:

  ```css
  :root { --ink: #16324f; --paper: #f7f8f6; --surface: #fff; --ledger: #5e7683; --rule: #d7dfe2; --verified: #0f766e; --amber: #b45309; --sans: ui-sans-serif, system-ui, sans-serif; --mono: ui-monospace, SFMono-Regular, Menlo, monospace; --serif: Georgia, serif; }
  * { box-sizing: border-box; }
  html { background: var(--paper); color: var(--ink); font-family: var(--sans); line-height: 1.5; }
  body { margin: 0; min-width: 20rem; }
  a { color: inherit; text-underline-offset: .18em; }
  a:focus-visible, button:focus-visible, input:focus-visible, textarea:focus-visible { outline: 3px solid var(--verified); outline-offset: 3px; }
  .skip-link { left: 1rem; position: fixed; top: -5rem; z-index: 2; background: var(--ink); color: var(--surface); padding: .6rem .8rem; }
  .skip-link:focus { top: 1rem; }
  .masthead { align-items: center; background: var(--surface); border-bottom: 1px solid var(--rule); display: flex; justify-content: space-between; min-height: 4.25rem; padding: 0 max(1.25rem, calc((100vw - 72rem) / 2)); }
  .wordmark, .receipt-strip, .eyebrow { font-family: var(--mono); font-size: .73rem; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
  .wordmark { text-decoration: none; }
  .primary-nav { align-items: center; display: flex; gap: 1.1rem; }
  .primary-nav a { text-decoration: none; }
  .primary-nav [aria-current="page"] { text-decoration: underline; text-decoration-thickness: 2px; }
  .nav-action, button { background: var(--ink); border: 1px solid var(--ink); color: var(--surface); cursor: pointer; font: 700 .78rem var(--mono); letter-spacing: .04em; padding: .7rem .9rem; text-transform: uppercase; }
  .page-shell { margin: 0 auto; max-width: 72rem; padding: clamp(2.25rem, 7vw, 5.5rem) max(1.25rem, 4vw); }
  .page-intro { max-width: 44rem; }
  .eyebrow, .receipt-strip { color: var(--ledger); margin: 0; }
  h1 { font-family: var(--serif); font-size: clamp(2.3rem, 6vw, 4.9rem); letter-spacing: -.045em; line-height: .98; margin: .6rem 0 1rem; }
  .page-intro > p:last-child { color: var(--ledger); font-size: 1.08rem; max-width: 38rem; }
  .route-grid, .form-grid { display: grid; gap: 1rem; grid-template-columns: repeat(2, minmax(0, 1fr)); margin-top: 2rem; }
  .route-card, .record, .upload-form { background: var(--surface); border: 1px solid var(--rule); }
  .route-card { display: grid; gap: .65rem; min-height: 12rem; padding: 1.25rem; text-decoration: none; }
  .route-card strong, .record strong { font-size: 1.1rem; }
  .route-card span:last-child, .record-detail, .form-note, .form-footer p, .empty-state { color: var(--ledger); }
  .record-list { display: grid; gap: .65rem; list-style: none; margin: 2rem 0; max-width: 52rem; padding: 0; }
  .record { list-style: none; }
  .record > a, .record label { display: grid; gap: .35rem; padding: 1rem 1.1rem; }
  .record label { grid-template-columns: auto 1fr; }
  .selection-form > button { margin-top: .25rem; }
  .transcript-layout { align-items: start; display: grid; gap: 2.5rem; grid-template-columns: minmax(12rem, 15rem) minmax(0, 1fr); margin-top: 2rem; }
  .prompt-index { border-top: 1px solid var(--rule); font-size: .9rem; position: sticky; top: 1rem; }
  .prompt-index ol { padding-left: 1.1rem; }
  .prompt-index a { display: block; margin: .55rem 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .event-stream { display: grid; gap: .85rem; }
  .event-card { background: var(--surface); border: 1px solid var(--rule); border-left: 3px solid var(--ledger); padding: 1rem 1.1rem; scroll-margin-top: 1rem; }
  .event-user { border-left-color: var(--verified); }
  .event-card > header { align-items: center; display: flex; justify-content: space-between; gap: 1rem; }
  .copy-anchor { background: transparent; border-color: var(--rule); color: var(--ink); font-size: .68rem; padding: .38rem .5rem; }
  pre { background: #102b3a; color: #eaf4f3; margin: .85rem 0 0; overflow-wrap: anywhere; padding: 1rem; white-space: pre-wrap; }
  details { border-top: 1px solid var(--rule); margin-top: .85rem; padding-top: .85rem; }
  summary { cursor: pointer; font: 700 .78rem var(--mono); }
  .upload-form { margin-top: 2rem; max-width: 52rem; padding: 1.25rem; }
  .upload-form label { display: grid; gap: .5rem; }
  .upload-form label > span { font-weight: 700; }
  .upload-form em { color: var(--ledger); font-size: .8rem; font-style: normal; font-weight: 400; }
  input, textarea { background: var(--surface); border: 1px solid #91a6b2; border-radius: 0; color: var(--ink); font: 1rem var(--sans); min-height: 2.7rem; padding: .6rem .7rem; width: 100%; }
  input[type="file"] { border-style: dashed; font: .84rem var(--mono); }
  textarea { min-height: 7rem; resize: vertical; }
  .full-width { grid-column: 1 / -1; }
  .form-footer { align-items: center; border-top: 1px solid var(--rule); display: flex; gap: 1rem; justify-content: space-between; margin-top: 1.25rem; padding-top: 1rem; }
  @media (max-width: 700px) { .masthead { align-items: flex-start; flex-direction: column; gap: .85rem; padding-block: 1rem; } .primary-nav { width: 100%; } .primary-nav a { padding-block: .35rem; } .primary-nav .nav-action { margin-left: auto; } .route-grid, .form-grid, .transcript-layout { grid-template-columns: 1fr; } .prompt-index { position: static; } .form-footer { align-items: flex-start; flex-direction: column; } .form-footer button { width: 100%; } }
  @media (prefers-reduced-motion: reduce) { *, *::before, *::after { scroll-behavior: auto !important; transition-duration: .01ms !important; animation-duration: .01ms !important; } }
  ```

  Keep `app.js` unchanged unless adding a brief `Copied` acknowledgement after a
  successful clipboard write; it must not change the URL, request, or button
  behavior when `navigator.clipboard` is unavailable.

- [ ] **Step 4: Verify assets and the full web package**

  Run:

  ```sh
  GOCACHE=$(pwd)/.go-cache go test ./internal/web -count=1
  GOCACHE=$(pwd)/.go-cache go test -race -count=1 ./...
  GOCACHE=$(pwd)/.go-cache go vet ./...
  GOCACHE=$(pwd)/.go-cache go build -o ./bin/agent-transcripts ./cmd/agent-transcripts
  git diff --check
  ```

  Expected: all commands exit 0. The static-asset test confirms content type and
  security headers remain fixed while the new assertion confirms the design,
  focus, reduced-motion, and mobile selectors are embedded.

- [ ] **Step 5: Perform a visual smoke test when loopback binding is available**

  Run:

  ```sh
  ./bin/agent-transcripts serve --open
  ```

  Expected: Home, Live, Library, Upload, and a transcript page remain reachable;
  navigation wraps below 700px, upload controls remain native, and prompt links
  still jump to event anchors. If the environment denies loopback binding, record
  the denial and rely on the handler and asset tests rather than weakening them.

- [ ] **Step 6: Commit the visual system**

  ```sh
  git add internal/web/static/app.css internal/web/static/app.js internal/web/server_test.go
  git commit -m "feat: style evidence ledger frontend"
  ```

## Final Verification Checklist

- [ ] `go test -race -count=1 ./...` passes with a workspace-local `GOCACHE`.
- [ ] `go vet ./...` passes with the same cache.
- [ ] `go build -o ./bin/agent-transcripts ./cmd/agent-transcripts` succeeds.
- [ ] `git diff --check` produces no output.
- [ ] Home, Live, Library, Upload, and transcript routes retain their existing server tests and form contracts.
- [ ] The UI has no dependency on JavaScript for navigation, import, or upload.
