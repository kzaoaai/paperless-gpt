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
    `TestCreatedDatePromptReceivesOriginalCreatedDate`.
  - `PDF_UPLOAD_MODE=version` (upstream PR icereed/paperless-gpt#1125, open): `UploadDocumentVersion` in `paperless.go`,
    `uploadProcessedPDFAsVersion` and `versionUnconfirmedError` in `ocr.go`, `UploadMode` on
    `OCROptions`/`OCRRun`, and the Playground/Activity wording in `web-app/src/components/ocr/`.
    Tests: `ocr_version_upload_test.go`. Upstream edits to `uploadProcessedPDF` or
    `ProcessDocumentOCR`'s upload switch will conflict here.
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
    the README table. Tests: `TestGenAIClientConfig`.
  - The Dockerfile's builder stage pins `MUSL_DEV_VERSION="1.2.6-r3"`. Alpine 3.24 dropped
    `-r2`, which upstream still pins, so its `apk add` fails and no image builds. On a conflict,
    take whichever version the current Alpine repo serves; when upstream's Renovate catches up,
    take theirs and drop this item.

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

### Open items

- Production runs `136277a` (deployed 2026-10-24): the Vertex backend, on Vertex with
  `gemini-3-flash-preview`. Verified by re-running one document with no API key in the container.
  Not yet verified: that the Developer Program credit absorbs the Vertex charges (Cloud billing
  reports).
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
- `PDF_UPLOAD_MODE=version` is upstream PR icereed/paperless-gpt#1125 (open). Once it merges,
  drop it from the sync checklist above.
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
- `gofmt -l .` lists several upstream files; leave them to upstream.
