@AGENTS.md

## This fork (kzaoaai/paperless-gpt)

AGENTS.md above is upstream's guide, kept verbatim so upstream merges stay clean. This section
covers only what differs in the fork and how it is deployed. Upstream ships CLAUDE.md as a
symlink to AGENTS.md; here it is a regular file — on a sync conflict, keep this file.

### Build, test, deploy

- Build the frontend before any Go build or test: the binary embeds `web-app/dist`
  (`embedded_assets.go`). `cd web-app && npm install && npm run build`, then
  `go vet ./... && go test ./...` from the repo root. AGENTS.md's `cp -r dist ../` step does
  not apply: nothing reads a root `dist/`.
- Run the whole package. `TestTokenLimitInCreatedDateGeneration` panics when run on its own
  (it relies on a template another test sets) — an upstream test-isolation bug.
- CI (`.github/workflows/docker-build-and-push.yml`) is the fork's own: one amd64 build that
  pushes `ghcr.io/kzaoaai/paperless-gpt:latest` and `:<sha>` on every push to `main` that
  changes something other than Markdown (`paths-ignore: '**.md'`), plus `workflow_dispatch`.
  Images carry `org.opencontainers.image.version` (short commit) and `.revision` labels; the
  lab's image-update card reads them to show which commits an update spans. No arm64, manifest-merge or E2E jobs — AGENTS.md's CI section does not
  apply here.
- A push never deploys. Production runs the image as one service of a Komodo-managed compose
  stack (UI-defined: Komodo's DB is the master copy of the compose). Deploy = change the
  compose in Komodo if config changes, write the identical file on the host, then pull and
  recreate only this service with `docker compose -p <project> pull paperless-gpt` and
  `docker compose -p <project> up -d --no-deps paperless-gpt`. Never redeploy the whole stack
  for this: it also pulls paperless-ngx, which tracks a moving tag.
- After a push that builds an image, the pushing session records the review the image-update
  card needs, since it knows what changed: once the build is green, in the homelab-ops
  workspace run `bin/image-check.sh --updates` (only a full run refreshes the card), then
  `bin/update-review.sh paperless-ngx paperless-gpt SAFE|NEEDS_CHANGE|HOLD "<what changed>"
  ["<what to do>"]`. The verdict binds to that exact digest. It records a review; it does not
  deploy, which stays AA's call from the card.

### Syncing upstream

- `git fetch upstream && git merge upstream/main` — merge, never rebase (history is shared
  with origin). Build and test before pushing; the push is what publishes `:latest`.
- Re-check these fork deltas after every sync:
  - The CI workflow above — keep the fork's single build job.
  - `getSuggestedCreatedDate` passes the document's current created date as template key
    `CreatedDate`. Upstream lacks it; production's custom created-date prompt depends on it,
    and a missing map key renders as empty with no error. Guarded by
    `TestCreatedDatePromptReceivesOriginalCreatedDate`. Offered upstream as icereed/paperless-gpt#1226
    (open); once it merges, take upstream's version and drop this item.
  - `PDF_UPLOAD_MODE=version` is upstream since icereed/paperless-gpt#1125 (synced 2026-10-25).
    The fork keeps three additions on top of it: `VersionAdded` on `ProcessedDocument` (set
    when the version is confirmed; `saveVersionText` below depends on it), the `kept` PDF action
    (Go, `api.ts`, `RunResults.tsx`, the Activity tab), and a five-minute version wait
    (`taskPollAttempts = 60`; upstream waits one minute; guarded by
    `TestVersionWaitOutlastsABusyWorker`). Upstream edits to `ProcessDocumentOCR`'s upload
    switch or the task-poll constants will conflict here.
  - Once paperless-ngx confirms a new version (`ProcessedDocument.VersionAdded`),
    `saveVersionText` (`version_text.go`) writes the recognized text into the new version and
    into the original, content only (`SetDocumentContent`, `paperless.go`). A new version's
    content is paperless-ngx's own extraction of the searchable PDF, and paperless-ngx 3.0
    rebuilds a document's search index entry from its ORIGINAL's content on every update
    (`DocumentViewSet.update` → `add_or_update` without effective content). Called after a
    manual OCR job (`processJob`, `jobs.go`) and in the auto-OCR loop (`background.go`).
    Tests: `version_text_test.go`.
  - A digital PDF whose own text already holds the recognized text keeps it: no version, and
    the auto loop leaves its content alone (`keepsExistingText`, `existing_text.go`; PDF action
    `kept`). "Digital" means no page is covered by an image (`pdfrender.PageText`); "holds"
    means 90% of the recognized words, overall and in each script with 3+ words, so missing
    Arabic cannot hide behind English. Tests: `existing_text_test.go`.
  - `go.mod` replaces `github.com/gardar/ocrchestra` with the kzaoaai/ocrchestra fork
    (branch `feat/unicode-text-layer`) for the Unicode text layer. Keep the `replace` when
    upstream moves ocrchestra; rebase the fork branch onto the new version instead. Guarded by
    `textlayer_test.go`.
  - `go.mod` also replaces `github.com/phpdave11/gofpdi` with the kzaoaai/gofpdi fork (branch
    `fix/import-page-boxes`): upstream gofpdi (through v1.0.16) gives every imported page page 1's
    box and aspect ratio, which crops, distorts or blanks any page sized unlike page 1. A `replace`
    in ocrchestra does not reach this module, so this one must stay too. ocrchestra keeps each
    page's original size, scales the hOCR onto it, and fails with `pdfocr.ErrPageGeometry` when the
    hOCR page is turned (Document AI turns sideways scans upright). Each page is imported with its
    CropBox (its MediaBox when it has none): iOS scans crop part of the MediaBox away, and importing
    the MediaBox made the guard below refuse them. Guarded by `pdf_guard_test.go`.
  - Before a searchable PDF is uploaded, `checkRebuiltPDF` (`pdf_guard.go`) renders it and the
    original: page count, each page's size (1 pt) and ink (18 dpi) must match, since the text layer
    is invisible. A refusal, or `ErrPageGeometry`, is PDF action `skipped`; the text is still
    written. Never remove the check: a version that does not look like the original replaces what
    paperless-ngx shows and downloads.
  - When pdfcpu cannot split a PDF, or gofpdi cannot read it in `ApplyOCR`, the step is retried
    once on `normalizePDF`'s rewrite (`pdf_normalize.go`): an annotation `/DA` written as a name
    (iOS form-field detection) becomes a string, and the copy has a classic xref table, no object
    streams, and Flate instead of ASCII85/ASCIIHex/LZW/RunLength. pdfcpu rejects a name `/DA` even
    in relaxed mode (its default), up to v0.16.1. The original is never changed, and
    `checkRebuiltPDF` still compares against it. Called in `DownloadDocumentAsPDF`
    (`paperless.go`) and `ProcessDocumentOCR` (`ocr.go`). Tests: `pdf_normalize_test.go`.
  - `GOOGLEAI_BACKEND=vertex` sends the googleai LLM and vision calls to Vertex AI with
    Application Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS`) instead of an API key;
    project from `GOOGLEAI_VERTEX_PROJECT` or `GOOGLE_PROJECT_ID`, location
    `GOOGLEAI_VERTEX_LOCATION` (default `global`; `GOOGLE_LOCATION` is Document AI's `us`/`eu`
    and is deliberately not a fallback). Both providers build their client with
    `ocr.NewGenAIClient` (`ocr/google_ai_client.go`); upstream edits to either provider's
    `genai.NewClient` call will conflict here. The three variables are in `env_registry.go` and
    the README table. Tests: `TestGenAIClientConfig`. Offered upstream as
    icereed/paperless-gpt#1225 (open); once it merges, take upstream's version and drop this item.
  - `GOOGLEAI_THINKING_LEVEL` (`minimal`/`low`/`medium`/`high`) sets Gemini 3's thinking level
    for the googleai LLM provider (`thinkingConfig`, `app_llm_googleai.go`); it overrides
    `GOOGLEAI_THINKING_BUDGET`, since the API rejects both. Upstream has only the budget. The
    provider also logs each call's token usage at debug level. In `env_registry.go` and the
    README table. Tests: `TestGoogleAIThinkingConfig`. Not offered upstream.
  - The Dockerfile's builder stage pins `MUSL_DEV_VERSION="1.2.6-r3"`. Alpine 3.24 dropped
    `-r2`, which upstream still pins, so its `apk add` fails and no image builds. On a conflict,
    take whichever version the current Alpine repo serves; when upstream's Renovate catches up,
    take theirs and drop this item. Offered upstream as icereed/paperless-gpt#1224 (open).
  - The Tidy page (below) is fork-only and touches upstream files in exactly these places:
    `registerTidy(router, api, app)` in `main.go`, the `tidyApplyAliases` line in
    `UpdateDocuments` (`paperless.go`), two `TIDY_*` entries in `env_registry.go`, and the
    import, route and sidebar item in `App.tsx` and `Sidebar.tsx`. Everything else is in
    `tidy_*.go` and `web-app/src/{Tidy.tsx,components/tidy/}`. If upstream stops calling
    `UpdateDocuments` with the database, aliases silently stop working; `TestTidyApplyAliasesRealNameWins`
    covers the function, not the call site.

### Production invariants (config, not code)

- `PDF_UPLOAD=true` with `PDF_UPLOAD_MODE=version`, `PDF_REPLACE=false`: the Document AI
  searchable PDF becomes a new version of the same document. Never switch to `new` mode:
  it uploads a second document, and with replace it deletes the original along with its
  custom fields, document type, storage path and ASN.
- The text layer is drawn with ocrchestra's `pdfocr.UnicodeFont` (`textLayerFont`,
  `textlayer.go`), from the kzaoaai/ocrchestra fork (`replace` in `go.mod`). Upstream's
  default core font encodes only Latin-1 and turns Arabic into mojibake, which paperless
  would extract as the document's text. `unencodableWords` still skips the searchable PDF
  for any word with a character the layer cannot encode (beyond Unicode's Basic
  Multilingual Plane, e.g. emoji); a character the font merely has no glyph for (CJK, Thai,
  …) still extracts as itself and is kept. Never drop the `replace` or the font setting
  while upstream ocrchestra lacks the Unicode layer.
- `PDF_SKIP_EXISTING_OCR=true` only recognizes a text layer ocrchestra itself drew (an optional
  content group named `OCR Text`), i.e. a document paperless-gpt already versioned; it does not
  look at text. Digital PDFs and other tools' OCR layers are handled by the `kept` rule above.
- paperless-ngx's OCR mode (UI Settings → OCR; its database value overrides
  `PAPERLESS_OCR_MODE`) must stay `auto`. paperless parses every new version; under `redo`
  it would re-OCR it with Tesseract and search would use that text instead of the layer.
- `PUID`/`PGID` equal to the owner of the bind-mounted host dirs (1000). The entrypoint
  `chown -R`s `/app` to PUID:PGID (default 10001), which rewrites bind-mounted host dirs.
- Custom prompt templates are mounted over `/app/prompts` and replace `default_prompts/`, so
  template data keys are a contract: renaming or dropping one silently blanks it.
- `OCR_PROCESS_MODE=pdf` downloads the original file and splits it with pdfcpu, so non-PDF
  originals (photos, `.eml`, Office files) cannot be OCR'd. paperless-ngx workflows give the
  OCR trigger tag only to `*.pdf`; everything else goes straight to the LLM stage.
  `OCR_MAX_RETRIES` fail-tags anything that still slips through.
- `/app/db` (modification history and the OCR Activity sqlite) is a bind mount — keep it.
- The `AI` custom field is set by a paperless-ngx workflow ("paperless-gpt: mark AI, drop the
  completion tag": on Document Updated with `AUTO_TAG_COMPLETE`, set `AI` = true, remove the
  tag), not by the LLM. Keep `AI` out of the custom-field prompt and the selected fields, and
  keep `AUTO_TAG_COMPLETE` non-empty: the workflow triggers on it. Manual review-page runs get
  no completion tag and so no `AI`, by choice.
- `MANUAL_TAG` equals `AUTO_TAG` on purpose: it is the only tag left out of the LLM's tag
  candidates, so the model can never suggest the tag that triggers the LLM stage.
- The LLM runs on Vertex AI (`GOOGLEAI_BACKEND=vertex`, `LLM_MODEL=gemini-3-flash-preview`), so
  it bills as Cloud usage, which the Developer Program credit covers, not as AI Studio prepay,
  which ran out and fail-tagged documents with 402 on 2026-10-23. `GOOGLEAI_API_KEY` is commented
  out. `gemini-2.5-flash` and `2.5-flash-lite` return 404 on Vertex for this project. Flash-Lite
  models were measured worse (wrong created dates, invented amounts, wrong tags), so they are not
  a cost saving worth taking.

### Tidy page (fork-only, `/tidy`)

Inventory and cleanup of paperless-ngx tags, correspondents, document types, storage paths and
custom fields. Go: `tidy_paperless.go` (API calls), `tidy_findings.go` (inventory, references,
findings), `tidy_actions.go` (actions, log, undo, aliases), `tidy_routes.go` (`/api/tidy/...`),
`tidy_llm.go` (LLM duplicate groups), `tidy_hygiene.go` (archive health, custom-field values),
`tidy_split.go` (splitting a catch-all correspondent). Tests: `tidy_test.go` and `tidy_v11_test.go`,
against a stateful fake paperless that applies paperless-ngx 3.0's tag-tree rules.

- **Read-only unless `TIDY_WRITES=true`.** :8080 has no login, and these are the first actions
  here that delete things. `TIDY_VIEWER_USER` (default `mcp-reader`) is the user the
  "hidden from" finding and the grant-view fix are about.
- Inventory: one pass over every root document (paperless-ngx 3.0's list leaves file-version rows
  out) gives each object's count and newest `created`/`added`. Findings: no documents and nothing
  using it (queue and marker tags are empty by design, so in-use ones are not listed); near-duplicate
  names (same name once case, punctuation, `&`/`and` and Inc/LLC/Co/The are dropped; an acronym equal
  to another name's initials; one edit apart, two for long names); a matching rule that needs text
  but has none (never matches); matching text that None/Automatic ignores; owned objects the viewer
  cannot see; references to deleted objects.
- References: paperless-ngx workflows (every trigger/action field that names a kind, plus custom
  field queries and `assign_custom_fields_values`), saved views (filter rules, custom-field columns
  and sort), mail rules, the inbox flag, and paperless-gpt's own config (`MANUAL_TAG`, `AUTO_TAG`,
  `AUTO_TAG_COMPLETE`, `AUTO_OCR_TAG`, `FAIL_TAG`, `PDF_OCR_COMPLETE_TAG`, its AI workflows' tags and
  custom fields, `custom_fields_selected_ids`). Merge and delete are refused while the source is
  referenced, and also when the references could not be read: deleting an object silently drops it
  from workflows. Renaming a tag paperless-gpt names in its config is refused too (it matches by name).
- Merge A into B: log first (A's full JSON with matching, owner and permissions; every document's
  before state), then for tags move A's children under B **before** moving documents (bulk
  `modify_tags` removing A also removes A's descendants), bulk-edit the documents, read them back
  (after state), re-query A, and delete A only when no document uses it; otherwise the action is
  `partial` and A stays. A tag cannot be merged into its own descendant. Correspondent and document
  type merges add an alias (`TidyAlias`): `tidyApplyAliases` adds merged-away names to the name maps
  `UpdateDocuments` resolves LLM suggestions with, so the LLM saying "ACCU" lands on the merge target
  instead of recreating it. A real object with that name always wins. Tag aliases do not exist: tag
  suggestions are filtered against existing tags before `UpdateDocuments`.
- Archive health (`tidyBuildHygiene`), from the same document pass: no correspondent, no type, no
  tags, still in an inbox tag after 14 days, carrying `AUTO_TAG` or `AUTO_OCR_TAG` for more than a
  day, carrying `FAIL_TAG`. Each links to the paperless-ngx list (`correspondent__isnull=1`,
  `is_tagged=0`, `is_in_inbox=1&added__date__lt=…`, which its UI reads from the URL). A "no text"
  count was dropped: paperless-ngx ignores an empty `content__iexact`, and reading every document's
  text on each refresh is too heavy.
- Custom-field values (`tidyBuildFieldAudit`): per field, documents carrying it and how many are
  empty, distinct values, the 25 most common (select ids shown as labels), values that differ only in
  case, spacing or punctuation (`"2025, 2026"` vs `"2025,2026"`), unused select options, monetary
  values without a currency code. Read-only.
- Split a catch-all correspondent (`tidy_split.go`): a background run (one at a time, stoppable,
  stored in `tidy_split_runs`) asks the same correspondent prompt document processing uses
  (`getSuggestedCorrespondent` with `filterCorrespondentsForPrompt` and the blacklist) about each
  document, by default with the bucket left out of the choices. The page groups the answers as
  `UpdateDocuments` would resolve them (case-insensitive, aliases): an existing correspondent, a new
  name (flagged when it looks like an existing one), or "stays" (the bucket itself, "Unknown"). A
  ticked group can be pointed at any existing correspondent instead. Applying is one `split` action:
  documents still on the bucket move; new names are created the way paperless-gpt creates them
  (owner = the API user) and get the viewer's view grant. Undo moves them back and deletes the
  correspondents the split created once they are empty and unused. A run costs one LLM call per
  document; a restart marks a running run `stopped`. Verified 2026-11-15 on the test instance: a
  split to two existing correspondents and one new one, then its undo, left the instance identical
  by name; the new correspondent was owned by the API user with the viewer's grant. The prompt side
  has only run on a stub and on local Ollama, which crashed after 4 of 7 documents. Never test it
  against Ollama on AA's Mac: `qwen3:8b` ran the M4 out of memory and macOS killed his apps.
- bulk_edit sends Document Updated for every moved document (`bulk_update_documents`), so paperless-ngx
  Document Updated workflows run on them; the merge preview lists the enabled ones.
- Undo: merges and deletes recreate the object from its JSON (`set_permissions`, so a view grant comes
  back; it gets a new id), move child tags back, and put each document back: tags exactly as before by
  per-document PATCH (a bulk "remove B" would strip B's descendants), other kinds by bulk edit.
  Documents changed since are refused unless forced, and then skipped. Undo refuses when an object
  with the old name exists again. Renames, matching changes and grants restore the logged values.
  Not undone: an older alias that a merge re-pointed from A to B. "Delete all" on the no-documents finding is one `delete_many` action over the ones with nothing
  referring to them (and, for tags, nothing below them); its undo recreates each, leaving any whose
  name exists again.
- Matching text that None/Automatic ignores can be switched to "Exact match" (the whole text as a
  phrase) or "Any word" (the page warns when a word like "of" would match nearly everything), or
  cleared. Production sets `AUTO_GENERATE_CORRESPONDENTS` and `AUTO_GENERATE_DOCUMENT_TYPE` and not
  `PRESERVE_EXISTING_METADATA`, so paperless-gpt's LLM overrides paperless-ngx's own matching for
  correspondents and types on every processed document; their matching rules matter only for what
  the LLM leaves unset.
- A trashed document that uses a merged or deleted object should lose it, since paperless-ngx clears
  the reference on delete (inferred from its models, not tested); restoring it from the trash would
  not bring it back. Version rows lose a merged tag the same way, which is harmless (they are not
  shown).
- Verified 2026-11-14 against a throwaway paperless-ngx 3.0 beta (the production image digest) seeded
  with production's object names and matching rules and 193 dummy documents: every action, then every
  undo, left objects, owners, permissions, matching, the tag tree and all document assignments
  identical by name; an LLM-style suggestion of a merged-away correspondent name landed on the target
  through `/api/update-documents`; LLM groups ran on local Ollama `qwen3:8b`. In production
  (2026-11-14, read-only): the inventory reads 1,199 documents in about 2 s, a merge preview works,
  and Gemini's duplicate groups matched the name rules (one pair). Writes are on since that day;
  merges and deletes have run in production, a split has not.

### Open items

- Production runs `03162b2` (deployed 2026-11-15: Tidy's Delete all and matching-text choices; rollback
  image `:59b98de6f77e38dc998eb63e633c070bbe73ca50`, Tidy v1.1; before it `1d87a5a`, the first Tidy), with
  `TIDY_WRITES: "true"` in the compose since 2026-11-14. No split has run in production yet. A database backup taken right before writes were enabled is on the CT; the homelable
  page's Changelog has the path and the restore command. Before it, `30ac1b1`
  (deployed 2026-10-31) with `GOOGLEAI_THINKING_LEVEL=low`; before that,
  `9068471` (deployed 2026-10-25): the 2026-10-25 upstream sync on top of the Vertex backend, on
  Vertex with `gemini-3-flash-preview`. Verified by re-running one document's
  LLM stage (title, tags, custom fields and the `AI` workflow all applied, no fail tag).
  The Developer Program credit does absorb Vertex charges: October 2026's report shows $9.66 of
  it against $15.70 of Vertex usage, almost all from the 2026-10-31 document-link backfill
  (1,190 custom-field calls, ~1.3¢ each).
- Thinking tokens are most of the cost: on 8 documents the custom-field prompt averaged ~2,400
  thinking tokens per call at the model's default level ($0.0094 a call) against ~0 at `low`
  ($0.0027, 71% less, about 5× faster). Answers at `low` were close; it fills `Amount` a little
  more readily (a loan estimate's loan amount, an insurance premium). Production sets
  `GOOGLEAI_THINKING_LEVEL=low` since 2026-10-31.
- `0e8da29`'s CropBox import was verified live on an iOS scan with a CropBox (manual job, PDF
  action `versioned`): the new page has the CropBox's size, and the text layer's words sit over
  the printed words, so Document AI's hOCR of such a page describes the CropBox.
- The OCR page's `auto_queue_count` counts every document tagged with `AUTO_OCR_TAG`, including
  paperless-ngx version rows (auto-matching stamps the tag on them) and trashed documents, so it
  overstates the queue (85 tagged, none of them a live root document, on 2026-10-11). The auto
  loop does not process those rows. Cosmetic; not requested.
- Sideways scans get no searchable PDF (`pdfocr.ErrPageGeometry`). Placing their words would need
  the direction Document AI turned the page; its response probably carries it (unchecked), but
  `gdocai` does not keep it in the hOCR. Not requested.
- The gofpdi fix is not offered upstream (phpdave11/gofpdi, inactive), by AA's choice; the
  `replace` stays until upstream fixes `ImportPage`/`UseTemplate`.
- `new` upload mode still copies no `document_type`, `custom_fields`, `storage_path` or
  `archive_serial_number` to the new document (`uploadProcessedPDF`, `ocr.go`); `version`
  mode avoids the problem because metadata never leaves the document.
- In the auto-OCR loop (`background.go`), a failed searchable-PDF upload still completes the
  run: the Document AI text is saved, the document leaves the OCR queue and the LLM stage
  runs; only the Activity entry shows "Searchable PDF failed". Deliberate: retrying would
  re-run Document AI and end in `paperless-gpt-failed` without LLM filling.
- Left as is on purpose: `app_llm_googleai.go` treats an empty Gemini answer as an error, so a
  document whose tag generation keeps coming back empty is fail-tagged after
  `AUTO_TAG_MAX_RETRIES`. So far that has only happened to documents with no real text, which
  need a human anyway, and an empty answer can also mean a safety block or exhausted output
  tokens. If it ever hits a real document: put the candidate's `FinishReason` in the error, and
  treat an empty answer as "no tags" only in the tag step and only when it is `STOP`.
  Upstream's empty-reply fix (2026-10 sync) covers only langchaingo's Anthropic and OpenAI
  errors, not this provider. Since the same sync the googleai provider runs behind
  `RateLimitedLLM`, so each empty answer is retried `LLM_MAX_RETRIES` (3) times within one
  suggestion call before it counts against `AUTO_TAG_MAX_RETRIES`.
- `pdf` mode could send non-PDF originals to one whole-document OCR call (as `whole_pdf`
  does) instead of failing. Currently handled by the workflow routing above.
- A version paperless-ngx has not confirmed within 5 minutes keeps paperless-ngx's own
  `pdftotext -layout` reading of the text layer (which misplaces signs and spaces next to
  numbers in right-to-left lines), and search keeps the original's text: a manual job then
  writes no text (its run shows a warning), and the auto loop's content update may land on
  the document before the version exists.
- ocrchestra fork (`feat/unicode-text-layer`): upstream `gardar/ocrchestra` is dormant (no push
  since 2025-05) and has an open PR (#12) with the same font change but no right-to-left
  handling. Offered there as a comment instead of a competing PR; waiting on the maintainer.
  Upstream paperless-gpt still pins gardar's original, so this fork's `replace` is what keeps
  the Arabic text layer correct.
- Documents that get no searchable PDF today, each for its own reason: a page Document AI
  turned upright (a sideways scan) is refused by ocrchestra, since its words cannot be placed
  without knowing which way it was turned; Document AI can
  reject a single page as corrupt, which fails the whole run; documents over
  `OCR_LIMIT_PAGES` (30) are skipped. Left as is by AA's choice. gofpdi's xref-stream limit
  ("/Columns <= 4") should now be covered by the `normalizePDF` retry (no sample to verify).
- Manual OCR jobs that crash in the searchable-PDF step (deferred by AA, Ops Backlog): docs 659
  and 665 panic with a nil pointer dereference in "apply OCR", 1449 panics with "Failed to get
  page content: Failed to resolve ..." (both from the PDF writer, gofpdi/ocrchestra side,
  unconfirmed), and 772 fails "reading page sizes: Failed to initialize parser". Seen in the
  2026-10-31 text-layer run; reproduce with `POST /api/documents/<id>/ocr` on one of them.
- `gofmt -l .` lists several upstream files; leave them to upstream.
