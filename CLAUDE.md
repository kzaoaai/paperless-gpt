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
  pushes `ghcr.io/kzaoaai/paperless-gpt:latest` and `:<sha>` on every push to `main`, plus
  `workflow_dispatch`. No arm64, manifest-merge or E2E jobs — AGENTS.md's CI section does not
  apply here.
- A push never deploys. Production runs the image as one service of a Komodo-managed compose
  stack (UI-defined: Komodo's DB is the master copy of the compose). Deploy = change the
  compose in Komodo if config changes, write the identical file on the host, then pull and
  recreate only this service with `docker compose -p <project> pull paperless-gpt` and
  `docker compose -p <project> up -d --no-deps paperless-gpt`. Never redeploy the whole stack
  for this: it also pulls paperless-ngx, which tracks a moving tag.

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

### Open items

- Production runs `b3e406b` (deployed 2026-10-06, including the upstream merge `6e8098c`). It had
  processed no real document yet at deploy time: the first scan through the new PDF library
  (go-pdfium 1.21.1) is its real test.
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
- Documents that get no searchable PDF today, each for its own reason: ocrchestra's `ApplyOCR`
  (gofpdi) cannot read some xref streams ("/Columns <= 4") and fails the PDF — rewriting the
  original with pdfcpu (xref table) before `ApplyOCR` would likely fix it; Document AI can
  reject a single page as corrupt, which fails the whole run; documents over
  `OCR_LIMIT_PAGES` (30) are skipped. Left as is by AA's choice.
- `gofmt -l .` lists several upstream files; leave them to upstream.
