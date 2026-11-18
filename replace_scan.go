package main

// Replace mode for new scans (fork-only, AUTO_OCR_REPLACE).
//
// In the auto-OCR queue, a new scan's searchable PDF replaces the document
// instead of becoming a second version of it: the PDF is uploaded as a new
// document, paperless-gpt waits until paperless-ngx confirms it was consumed,
// gives it what the scan carries at that moment (title, created date, tags,
// correspondent, type, storage path, owner and permissions) plus the recognized
// text as its content, and only then deletes the scan (paperless-ngx keeps it
// in its trash). The LLM stage then runs once, on the replacement.
//
// Every step is recorded in ocr_replacements before it is taken, so a restart
// or a failed request resumes where it stopped instead of uploading again or
// sending the replacement back through OCR. Nothing is deleted on anything less
// than a confirmed consume and a hand-over that reads back as written.
//
// Only documents that are plainly new scans qualify (replaceEligibility); any
// other document in the queue, and every manual OCR job, keeps version mode.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gardar/ocrchestra/pkg/pdfocr"
	"github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// PDFUploadModeReplace is the auto-OCR loop's upload mode for a document that
// qualifies for replacement. It is never read from PDF_UPLOAD_MODE: manual jobs
// and the configured default stay on version mode.
const PDFUploadModeReplace = "replace"

// pdfActionReplaceReady marks a ProcessedDocument whose searchable PDF is
// ready and waits for the auto-OCR loop to replace the document with it. It is
// never stored: the loop turns it into the run's outcome.
const pdfActionReplaceReady = "replace-ready"

var (
	// autoOCRReplace turns replace mode on (AUTO_OCR_REPLACE=true). It needs
	// PDF_UPLOAD=true and PDF_UPLOAD_MODE=version, the mode everything that
	// does not qualify keeps.
	autoOCRReplace = os.Getenv("AUTO_OCR_REPLACE") == "true"
	// autoOCRReplaceMaxAge: a document added longer ago than this is not a new
	// scan and keeps version mode (AUTO_OCR_REPLACE_MAX_AGE).
	autoOCRReplaceMaxAge = 7 * 24 * time.Hour
	// replacePendingLimit: how long an unconfirmed upload keeps its scan in the
	// queue before the scan is marked failed for a person to look at.
	replacePendingLimit = time.Hour
	// replaceWatchLimit: how long a given-up upload is still watched, so a
	// replacement that arrives late is marked too instead of passing as new.
	replaceWatchLimit = 7 * 24 * time.Hour
)

// loadAutoOCRReplaceConfig reads AUTO_OCR_REPLACE_MAX_AGE and checks that
// replace mode has what it needs. Called once at startup.
func loadAutoOCRReplaceConfig() error {
	if raw := strings.TrimSpace(os.Getenv("AUTO_OCR_REPLACE_MAX_AGE")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid AUTO_OCR_REPLACE_MAX_AGE %q: a positive duration such as 168h", raw)
		}
		autoOCRReplaceMaxAge = d
	}
	if !autoOCRReplace {
		return nil
	}
	if !pdfUpload || pdfUploadMode != PDFUploadModeVersion {
		return fmt.Errorf("AUTO_OCR_REPLACE=true needs PDF_UPLOAD=true and PDF_UPLOAD_MODE=%s (the mode manual jobs and documents that are not new scans keep)", PDFUploadModeVersion)
	}
	if pdfReplace {
		return fmt.Errorf("AUTO_OCR_REPLACE=true cannot be combined with PDF_REPLACE=true")
	}
	return nil
}

// Replacement states. "uploading", "uploaded", "consumed" and "handed_over"
// are open: the scan is still in the queue and the loop resumes from there.
const (
	replaceUploading  = "uploading"   // record written, upload not confirmed sent
	replaceUploaded   = "uploaded"    // paperless-ngx accepted the upload (task_id)
	replaceConsumed   = "consumed"    // the replacement exists (replacement_id)
	replaceHandedOver = "handed_over" // the replacement carries the scan's metadata and text
	replaceDone       = "done"        // the scan is in paperless-ngx's trash
	replaceRejected   = "rejected"    // paperless-ngx refused the upload; the scan was kept
	replaceGaveUp     = "gave_up"     // no confirmation in time; the scan was marked failed
	replaceLate       = "late"        // a given-up upload arrived after all and was marked failed
)

var openReplaceStates = []string{replaceUploading, replaceUploaded, replaceConsumed, replaceHandedOver}

// OCRReplacement records one scan being replaced by its searchable PDF.
type OCRReplacement struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	OriginalID    int       `gorm:"index;not null" json:"original_id"`
	ReplacementID int       `gorm:"index" json:"replacement_id,omitempty"`
	JobID         string    `gorm:"size:64" json:"job_id"`
	TaskID        string    `gorm:"size:64" json:"task_id,omitempty"`
	State         string    `gorm:"size:16;index" json:"state"`
	Text          string    `json:"-"` // the recognized text, until the hand-over is done
	Detail        string    `json:"detail,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func ensureReplacementTable(db *gorm.DB) error {
	if db.Migrator().HasTable(&OCRReplacement{}) {
		return nil
	}
	return db.AutoMigrate(&OCRReplacement{})
}

// replaceClient is what replace mode needs beyond ClientInterface: the
// document's raw fields (ids, owner, permissions) and a plain PATCH.
// PaperlessClient implements it; a client that does not (test mocks) keeps
// every document on version mode.
type replaceClient interface {
	GetDocumentFields(ctx context.Context, documentID int) (map[string]interface{}, error)
	PatchDocumentFields(ctx context.Context, documentID int, fields map[string]interface{}) error
	CountDocumentsLinkingTo(ctx context.Context, fieldID, documentID int) (int, error)
}

// GetDocumentFields returns a document as paperless-ngx sends it, with
// permissions (?full_perms=true).
func (client *PaperlessClient) GetDocumentFields(ctx context.Context, documentID int) (map[string]interface{}, error) {
	resp, err := client.Do(ctx, "GET", fmt.Sprintf("api/documents/%d/?full_perms=true", documentID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("error fetching document %d: %w", documentID, ErrDocumentNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("error fetching document %d: %d, %s", documentID, resp.StatusCode, string(body))
	}
	var fields map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&fields); err != nil {
		return nil, fmt.Errorf("error parsing document %d: %w", documentID, err)
	}
	return fields, nil
}

// PatchDocumentFields sends fields as one PATCH of the document.
func (client *PaperlessClient) PatchDocumentFields(ctx context.Context, documentID int, fields map[string]interface{}) error {
	body, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("error encoding the update of document %d: %w", documentID, err)
	}
	resp, err := client.Do(ctx, "PATCH", fmt.Sprintf("api/documents/%d/", documentID), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("error updating document %d: %w", documentID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("error updating document %d: %d, %s", documentID, resp.StatusCode, string(b))
	}
	return nil
}

// CountDocumentsLinkingTo counts documents whose document-link custom field
// fieldID points at documentID.
func (client *PaperlessClient) CountDocumentsLinkingTo(ctx context.Context, fieldID, documentID int) (int, error) {
	query, err := json.Marshal([]interface{}{fieldID, "contains", []int{documentID}})
	if err != nil {
		return 0, err
	}
	path := "api/documents/?fields=id&page_size=1&custom_field_query=" + url.QueryEscape(string(query))
	resp, err := client.Do(ctx, "GET", path, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("error counting links to document %d: %d, %s", documentID, resp.StatusCode, string(b))
	}
	var page struct {
		Count *int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil || page.Count == nil {
		return 0, fmt.Errorf("error parsing the links to document %d", documentID)
	}
	return *page.Count, nil
}

func (app *App) replaceClient() (replaceClient, bool) {
	rc, ok := app.Client.(replaceClient)
	return rc, ok
}

// replaceEligibility says whether a queued document is a new scan that may be
// replaced, and if not, why. Anything it cannot check counts as "no": the
// document then keeps version mode, which loses nothing.
func (app *App) replaceEligibility(ctx context.Context, documentID int) (bool, string) {
	rc, ok := app.replaceClient()
	if !ok {
		return false, "the paperless-ngx client cannot read the fields replace mode needs"
	}
	if app.Database == nil {
		return false, "no database to record the replacement in"
	}
	doc, err := rc.GetDocumentFields(ctx, documentID)
	if err != nil {
		return false, fmt.Sprintf("could not read the document: %v", err)
	}
	if root, _ := doc["root_document"].(float64); root != 0 {
		return false, "it is a version of another document"
	}
	if mime, _ := doc["mime_type"].(string); mime != "" && mime != "application/pdf" {
		return false, fmt.Sprintf("its file is %s, not a PDF", mime)
	}
	if notes, _ := doc["notes"].([]interface{}); len(notes) > 0 {
		return false, "it has notes"
	}
	if asn := doc["archive_serial_number"]; asn != nil {
		return false, "it has an archive serial number"
	}
	if cf, _ := doc["custom_fields"].([]interface{}); len(cf) > 0 {
		return false, "it has custom fields"
	}
	if versions, _ := doc["versions"].([]interface{}); len(versions) > 1 {
		return false, "it already has versions"
	}
	added, _ := doc["added"].(string)
	addedAt, err := time.Parse(time.RFC3339Nano, added)
	if err != nil {
		return false, fmt.Sprintf("its added date %q could not be read", added)
	}
	if age := time.Since(addedAt); age > autoOCRReplaceMaxAge {
		return false, fmt.Sprintf("it was added %s ago, longer than AUTO_OCR_REPLACE_MAX_AGE (%s)", age.Round(time.Minute), autoOCRReplaceMaxAge)
	}
	fields, err := app.Client.GetCustomFields(ctx)
	if err != nil {
		return false, fmt.Sprintf("could not read the custom fields: %v", err)
	}
	for _, f := range fields {
		if f.DataType != "documentlink" {
			continue
		}
		n, err := rc.CountDocumentsLinkingTo(ctx, f.ID, documentID)
		if err != nil {
			return false, fmt.Sprintf("could not check links to it: %v", err)
		}
		if n > 0 {
			return false, fmt.Sprintf("%d document(s) link to it in %q", n, f.Name)
		}
	}
	if err := ensureReplacementTable(app.Database); err != nil {
		return false, fmt.Sprintf("could not create the replacement records: %v", err)
	}
	var open int64
	if err := app.Database.Model(&OCRReplacement{}).Where("replacement_id = ? OR (original_id = ? AND state IN ?)", documentID, documentID, openReplaceStates).Count(&open).Error; err != nil {
		return false, fmt.Sprintf("could not read the replacement records: %v", err)
	}
	if open > 0 {
		return false, "it is part of a replacement already"
	}
	return true, ""
}

// hasSearchableLayer reports whether a PDF already carries a text layer this
// program drew. Such a PDF is never replaced again, whatever
// PDF_SKIP_EXISTING_OCR says, so a replacement can never start a loop.
func hasSearchableLayer(pdfData []byte) bool {
	if len(pdfData) == 0 {
		return false
	}
	cfg := pdfocr.DefaultConfig()
	cfg.Strict = true
	res, err := pdfocr.DetectOCR(pdfData, cfg)
	return err == nil && res.HasLayerOCR
}

// replaceOutcome is what replaceScan did with the scan.
type replaceOutcome int

const (
	// replaceFinished: the replacement carries everything and the scan is in
	// the trash. The scan must not be updated.
	replaceFinished replaceOutcome = iota
	// replaceOpen: the upload or hand-over is not finished; the scan stays in
	// the queue untouched and the next cycle resumes from the record.
	replaceOpen
	// replaceFellBack: nothing was replaced; the scan goes on as in version
	// mode (the caller completes it with the recognized text).
	replaceFellBack
)

// replaceScan uploads pd's searchable PDF as a new document and hands the scan
// over to it. pd.PDFAction and pd.PDFDetail are set to the outcome.
func (app *App) replaceScan(ctx context.Context, document Document, pd *ProcessedDocument, jobID string, logger *logrus.Entry) replaceOutcome {
	rc, ok := app.replaceClient()
	if !ok {
		app.versionInstead(ctx, document.ID, pd, logger, "replace mode is not available")
		return replaceFellBack
	}
	scan, err := rc.GetDocumentFields(ctx, document.ID)
	if err != nil {
		app.versionInstead(ctx, document.ID, pd, logger, fmt.Sprintf("the document could not be read before the upload (%v)", err))
		return replaceFellBack
	}
	rec := &OCRReplacement{OriginalID: document.ID, JobID: jobID, State: replaceUploading, Text: pd.Text}
	if err := app.Database.Create(rec).Error; err != nil {
		app.versionInstead(ctx, document.ID, pd, logger, fmt.Sprintf("the replacement could not be recorded (%v)", err))
		return replaceFellBack
	}

	metadata := map[string]interface{}{}
	if title, _ := scan["title"].(string); title != "" {
		metadata["title"] = title
	}
	if created, _ := scan["created"].(string); created != "" {
		metadata["created"] = created
	}
	taskID, err := app.Client.UploadDocument(ctx, pd.PDFData, replacementFileName(document.ID, scan), metadata)
	if err != nil && ctx.Err() != nil {
		// Stopped mid-upload: it may have reached paperless-ngx. The record
		// stays "uploading", so the next start marks the scan for a person
		// instead of uploading it a second time.
		pd.PDFAction = "failed"
		pd.PDFDetail = "paperless-gpt stopped while uploading the replacement; the scan was kept."
		return replaceOpen
	}
	if err != nil {
		app.closeReplacement(rec, replaceRejected, fmt.Sprintf("upload failed: %v", err))
		app.versionInstead(ctx, document.ID, pd, logger, fmt.Sprintf("uploading the replacement failed (%v)", err))
		return replaceFellBack
	}
	rec.TaskID = taskID
	rec.State = replaceUploaded
	app.saveReplacement(rec, logger)
	logger.WithField("task_id", taskID).Info("Searchable PDF uploaded as the scan's replacement; waiting for paperless-ngx to consume it")

	for attempt := 0; attempt < taskPollAttempts && rec.State == replaceUploaded; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				attempt = taskPollAttempts
				continue
			case <-time.After(taskPollInterval):
			}
		}
		if rejected := app.checkReplacementTask(ctx, rec, logger); rejected != "" {
			app.closeReplacement(rec, replaceRejected, rejected)
			app.versionInstead(ctx, document.ID, pd, logger, "paperless-ngx refused the replacement ("+rejected+")")
			return replaceFellBack
		}
	}
	if rec.State == replaceUploaded {
		pd.PDFAction = "attached"
		pd.PDFDetail = fmt.Sprintf("Uploaded as a replacement, but paperless-ngx had not confirmed it (task %s) when paperless-gpt stopped waiting. The scan stays in the queue unchanged; paperless-gpt checks again every cycle and marks it failed after %s.", taskID, replacePendingLimit)
		return replaceOpen
	}
	if err := app.advanceReplacement(ctx, rec, logger); err != nil {
		pd.PDFAction = "attached"
		pd.PDFDetail = fmt.Sprintf("The replacement (document %d) was consumed, but %v. The scan was kept and stays in the queue; paperless-gpt retries every cycle.", rec.ReplacementID, err)
		return replaceOpen
	}
	pd.PDFAction = "replaced"
	pd.ReplacedOriginal = true
	pd.PDFDetail = replacedDetail(rec)
	return replaceFinished
}

func replacedDetail(rec *OCRReplacement) string {
	return fmt.Sprintf("Replaced by document %d; the scan is in paperless-ngx's trash.", rec.ReplacementID)
}

// replacementFileName keeps the scan's own file name, so the replacement looks
// like what was uploaded; a name that is not a PDF's gets paperless-gpt's.
func replacementFileName(documentID int, scan map[string]interface{}) string {
	name, _ := scan["original_file_name"].(string)
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), ".pdf") {
		return strings.TrimSpace(name)
	}
	return fmt.Sprintf("%08d_paperless-gpt_ocr.pdf", documentID)
}

// versionInstead adds the searchable PDF as a new version, the way version
// mode does, after replacing was not possible.
func (app *App) versionInstead(ctx context.Context, documentID int, pd *ProcessedDocument, logger *logrus.Entry, why string) {
	logger.Warnf("Not replacing the scan: %s; adding the searchable PDF as a new version instead", why)
	err := app.uploadProcessedPDFAsVersion(ctx, documentID, pd.PDFData, logger)
	var unconfirmed *versionUnconfirmedError
	switch {
	case err == nil:
		pd.PDFAction = "versioned"
		pd.VersionAdded = true
		pd.PDFDetail = "Not replaced: " + why + "; added as a new version instead."
	case errors.As(err, &unconfirmed):
		pd.PDFAction = "versioned"
		pd.PDFDetail = fmt.Sprintf("Not replaced: %s. Added as a new version, but paperless-ngx had not confirmed it when paperless-gpt stopped waiting; check task %s in paperless-ngx.", why, unconfirmed.taskID)
	default:
		pd.PDFAction = "failed"
		pd.PDFDetail = fmt.Sprintf("Not replaced: %s; adding it as a new version failed too: %v", why, err)
	}
}

func (app *App) saveReplacement(rec *OCRReplacement, logger *logrus.Entry) {
	if err := app.Database.Save(rec).Error; err != nil {
		logger.WithError(err).Errorf("Could not record replacement state %q", rec.State)
	}
}

func (app *App) closeReplacement(rec *OCRReplacement, state, detail string) {
	rec.State = state
	rec.Detail = detail
	rec.Text = ""
	if err := app.Database.Save(rec).Error; err != nil {
		log.WithError(err).Errorf("Document %d: could not record replacement state %q", rec.OriginalID, state)
	}
}

// replacementDocumentID reads the id of the document a consume task created:
// related_document_ids (3.x), result_data.document_id (3.4) or
// related_document (2.x).
func replacementDocumentID(task map[string]interface{}) int {
	if ids, ok := task["related_document_ids"].([]interface{}); ok && len(ids) > 0 {
		if id, ok := ids[0].(float64); ok && id > 0 {
			return int(id)
		}
	}
	if data, ok := task["result_data"].(map[string]interface{}); ok {
		if id, ok := data["document_id"].(float64); ok && id > 0 {
			return int(id)
		}
	}
	switch v := task["related_document"].(type) {
	case float64:
		return int(v)
	case string:
		var id int
		if _, err := fmt.Sscanf(v, "%d", &id); err == nil {
			return id
		}
	}
	return 0
}

// checkReplacementTask polls an uploaded replacement's task once. On success
// it records the replacement's id (state consumed). On failure it returns
// paperless-ngx's reason, for the caller to fall back and close the record;
// otherwise "".
func (app *App) checkReplacementTask(ctx context.Context, rec *OCRReplacement, logger *logrus.Entry) string {
	task, err := app.Client.GetTaskStatus(ctx, rec.TaskID)
	if err != nil {
		logger.WithError(err).Debug("Could not read the replacement's task")
		return ""
	}
	status, _ := task["status"].(string)
	switch {
	case strings.EqualFold(status, "SUCCESS"):
		id := replacementDocumentID(task)
		if id == 0 {
			logger.Warnf("paperless-ngx finished task %s but named no document; checking again later", rec.TaskID)
			return ""
		}
		rec.ReplacementID = id
		rec.State = replaceConsumed
		app.saveReplacement(rec, logger)
		logger.WithField("replacement_id", id).Info("paperless-ngx consumed the replacement")
	case strings.EqualFold(status, "FAILURE"):
		return fmt.Sprint(taskResultDetail(task))
	}
	return ""
}

// advanceReplacement takes a consumed replacement through the hand-over and the
// scan's deletion, recording each step. It resumes from rec.State.
func (app *App) advanceReplacement(ctx context.Context, rec *OCRReplacement, logger *logrus.Entry) error {
	rc, ok := app.replaceClient()
	if !ok {
		return fmt.Errorf("replace mode is not available")
	}
	logger = logger.WithField("replacement_id", rec.ReplacementID)
	if rec.State == replaceConsumed {
		if err := app.handOver(ctx, rc, rec, logger); err != nil {
			rec.Detail = err.Error()
			app.saveReplacement(rec, logger)
			return err
		}
		rec.State = replaceHandedOver
		rec.Detail = ""
		app.saveReplacement(rec, logger)
	}
	if rec.State == replaceHandedOver {
		if err := app.Client.DeleteDocument(ctx, rec.OriginalID); err != nil && !strings.Contains(err.Error(), " 404,") {
			rec.Detail = fmt.Sprintf("deleting the scan failed: %v", err)
			app.saveReplacement(rec, logger)
			return fmt.Errorf("deleting the scan failed: %w", err)
		}
		app.closeReplacement(rec, replaceDone, "")
		if err := FinishOCRRun(app.Database, rec.JobID, "completed", "", 0, 0, "replaced", replacedDetail(rec)); err != nil {
			logger.WithError(err).Warn("Could not update the OCR run")
		}
		logger.Info("Scan replaced by its searchable PDF; the scan is in paperless-ngx's trash")
	}
	return nil
}

// handOver gives the replacement what the scan carries now (title, created
// date, correspondent, type, storage path, owner, permissions and tags, the
// queue tag swapped for the OCR-complete tag) and the recognized text, then
// reads it back. The scan's own matching, workflows and mail-rule assignments
// ran at its consume, so they are in what is copied; the replacement's own
// consume ran them again and is overwritten.
func (app *App) handOver(ctx context.Context, rc replaceClient, rec *OCRReplacement, logger *logrus.Entry) error {
	scan, err := rc.GetDocumentFields(ctx, rec.OriginalID)
	if err != nil {
		return fmt.Errorf("the scan could not be read: %w", err)
	}
	if _, err := rc.GetDocumentFields(ctx, rec.ReplacementID); err != nil {
		return fmt.Errorf("the replacement could not be read: %w", err)
	}
	allTags, err := app.Client.GetAllTags(ctx)
	if err != nil {
		return fmt.Errorf("the tags could not be read: %w", err)
	}
	tags := handOverTags(scan, allTags)
	if app.pdfOCRTagging && app.pdfOCRCompleteTag != "" {
		id, ok := lookupTagIDCaseInsensitive(allTags, app.pdfOCRCompleteTag)
		if !ok {
			if id, err = app.Client.CreateTag(ctx, app.pdfOCRCompleteTag); err != nil {
				return fmt.Errorf("the OCR complete tag could not be created: %w", err)
			}
		}
		if !slices.Contains(tags, id) {
			tags = append(tags, id)
		}
	}
	fields := map[string]interface{}{
		"content":       rec.Text,
		"title":         scan["title"],
		"created":       scan["created"],
		"correspondent": scan["correspondent"],
		"document_type": scan["document_type"],
		"storage_path":  scan["storage_path"],
		"tags":          tags,
		"owner":         scan["owner"],
	}
	if perms, ok := scan["permissions"].(map[string]interface{}); ok {
		fields["set_permissions"] = perms
	}
	if err := rc.PatchDocumentFields(ctx, rec.ReplacementID, fields); err != nil {
		return fmt.Errorf("the replacement could not be updated: %w", err)
	}
	got, err := rc.GetDocumentFields(ctx, rec.ReplacementID)
	if err != nil {
		return fmt.Errorf("the replacement could not be read back: %w", err)
	}
	if problem := handOverMismatch(fields, got); problem != "" {
		return fmt.Errorf("the replacement did not keep what was written (%s)", problem)
	}
	if err := InsertModification(app.Database, &ModificationHistory{
		DocumentID:    uint(rec.ReplacementID),
		ModField:      "content",
		PreviousValue: "(paperless-ngx's own extraction of the searchable PDF)",
		NewValue:      rec.Text,
	}); err != nil {
		logger.WithError(err).Warn("Could not record the replacement's content change")
	}
	logger.Info("Replacement carries the scan's metadata and the recognized text")
	return nil
}

func lookupTagIDCaseInsensitive(allTags map[string]int, name string) (int, bool) {
	if id, ok := allTags[name]; ok {
		return id, true
	}
	for n, id := range allTags {
		if strings.EqualFold(n, name) {
			return id, true
		}
	}
	return 0, false
}

// handOverTags is the scan's tag ids without the auto-OCR queue tag.
func handOverTags(scan map[string]interface{}, allTags map[string]int) []int {
	queue, hasQueue := lookupTagIDCaseInsensitive(allTags, autoOcrTag)
	tags := []int{}
	raw, _ := scan["tags"].([]interface{})
	for _, t := range raw {
		f, ok := t.(float64)
		if !ok {
			continue
		}
		id := int(f)
		if hasQueue && id == queue {
			continue
		}
		if !slices.Contains(tags, id) {
			tags = append(tags, id)
		}
	}
	return tags
}

// handOverMismatch compares what was written with what paperless-ngx returns.
// Content is compared without surrounding white space (paperless-ngx trims
// it); permissions are not compared, since the reply lists them per user.
func handOverMismatch(want map[string]interface{}, got map[string]interface{}) string {
	if strings.TrimSpace(fmt.Sprint(got["content"])) != strings.TrimSpace(fmt.Sprint(want["content"])) {
		return "content"
	}
	wantTags := append([]int(nil), want["tags"].([]int)...)
	var gotTags []int
	raw, _ := got["tags"].([]interface{})
	for _, t := range raw {
		if f, ok := t.(float64); ok {
			gotTags = append(gotTags, int(f))
		}
	}
	slices.Sort(wantTags)
	slices.Sort(gotTags)
	if !slices.Equal(wantTags, gotTags) {
		return fmt.Sprintf("tags %v, expected %v", gotTags, wantTags)
	}
	for _, key := range []string{"correspondent", "document_type", "storage_path", "owner", "created"} {
		if fmt.Sprint(got[key]) != fmt.Sprint(want[key]) {
			return fmt.Sprintf("%s %v, expected %v", key, got[key], want[key])
		}
	}
	return ""
}

// resumeReplacements runs at the start of every auto-OCR cycle: it moves open
// replacements on and returns the ids the cycle must leave alone (scans with
// an open replacement, and every replacement paperless-gpt uploaded).
func (app *App) resumeReplacements(ctx context.Context) (skip map[int]bool, handled int, errs []error) {
	skip = map[int]bool{}
	if app.Database == nil {
		return skip, 0, nil
	}
	if err := ensureReplacementTable(app.Database); err != nil {
		return skip, 0, []error{fmt.Errorf("replacement records: %w", err)}
	}
	var recs []OCRReplacement
	// Open records, and given-up uploads whose task is still watched.
	if err := app.Database.Where("state IN ? OR (state = ? AND task_id <> '')", openReplaceStates, replaceGaveUp).Order("id").Find(&recs).Error; err != nil {
		return skip, 0, []error{fmt.Errorf("replacement records: %w", err)}
	}
	for i := range recs {
		rec := &recs[i]
		logger := documentLogger(rec.OriginalID).WithField("task_id", rec.TaskID)
		if rec.State == replaceGaveUp {
			app.watchGivenUp(ctx, rec, logger)
			continue
		}
		switch rec.State {
		case replaceUploading:
			// The upload may or may not have reached paperless-ngx: uploading
			// again could make a duplicate, so a person has to look.
			app.giveUpReplacement(ctx, rec, logger, "paperless-gpt stopped while uploading the searchable PDF as its replacement; check paperless-ngx for a second copy of this document")
			continue
		case replaceUploaded:
			if reason := app.checkReplacementTask(ctx, rec, logger); reason != "" {
				app.completeScanWithText(ctx, rec, fmt.Sprintf("paperless-ngx refused the replacement (%s); the scan keeps its file and gets the recognized text", reason), logger)
				if rec.State == replaceUploaded {
					app.closeReplacement(rec, replaceRejected, reason)
				}
				handled++
				continue
			}
			if rec.State == replaceUploaded {
				if time.Since(rec.CreatedAt) > replacePendingLimit {
					app.giveUpReplacement(ctx, rec, logger, fmt.Sprintf("paperless-ngx did not confirm the searchable PDF's upload (task %s) within %s; nothing was deleted. If a second copy of this document arrives, it is marked failed too", rec.TaskID, replacePendingLimit))
				} else {
					skip[rec.OriginalID] = true
				}
				continue
			}
		}
		skip[rec.OriginalID] = true
		if rec.ReplacementID != 0 {
			skip[rec.ReplacementID] = true
		}
		if err := app.advanceReplacement(ctx, rec, logger); err != nil {
			errs = append(errs, fmt.Errorf("document %d replacement: %w", rec.OriginalID, err))
			continue
		}
		handled++
	}
	var ids []int
	if err := app.Database.Model(&OCRReplacement{}).Where("replacement_id <> 0").Pluck("replacement_id", &ids).Error; err == nil {
		for _, id := range ids {
			skip[id] = true
		}
	}
	return skip, handled, errs
}

// giveUpReplacement marks the scan failed (queue tag off, fail tag on, a
// record for the Tidy page) and keeps watching the upload's task.
func (app *App) giveUpReplacement(ctx context.Context, rec *OCRReplacement, logger *logrus.Entry, reason string) {
	scan, err := app.Client.GetDocument(ctx, rec.OriginalID)
	if err != nil {
		logger.WithError(err).Error("Could not read the scan to mark it failed; trying again next cycle")
		return
	}
	if err := markProcessingFailed(ctx, app.Client, app.Database, scan, autoOcrTag, reason); err != nil {
		logger.WithError(err).Error("Could not mark the scan failed; trying again next cycle")
		return
	}
	rec.State = replaceGaveUp
	rec.Detail = reason
	rec.Text = ""
	app.saveReplacement(rec, logger)
	if err := FinishOCRRun(app.Database, rec.JobID, "completed", "", 0, 0, "failed", "Not replaced: "+reason); err != nil {
		logger.WithError(err).Warn("Could not update the OCR run")
	}
}

// watchGivenUp checks a given-up upload's task. If the replacement arrived
// after all, it is marked failed too (it would otherwise pass as a new scan
// next to the kept one), so both show up for a person to choose.
func (app *App) watchGivenUp(ctx context.Context, rec *OCRReplacement, logger *logrus.Entry) {
	if time.Since(rec.CreatedAt) > replaceWatchLimit {
		rec.TaskID = "" // stop watching
		app.saveReplacement(rec, logger)
		return
	}
	task, err := app.Client.GetTaskStatus(ctx, rec.TaskID)
	if err != nil {
		return
	}
	status, _ := task["status"].(string)
	switch {
	case strings.EqualFold(status, "FAILURE"):
		rec.TaskID = ""
		app.saveReplacement(rec, logger)
	case strings.EqualFold(status, "SUCCESS"):
		id := replacementDocumentID(task)
		if id == 0 {
			return
		}
		doc, err := app.Client.GetDocument(ctx, id)
		if err != nil {
			return
		}
		reason := fmt.Sprintf("this is the searchable PDF of document %d, which arrived after paperless-gpt had given up on it; document %d was kept and marked failed. Keep one of the two", rec.OriginalID, rec.OriginalID)
		if err := markProcessingFailed(ctx, app.Client, app.Database, doc, autoOcrTag, reason); err != nil {
			logger.WithError(err).Error("Could not mark the late replacement failed; trying again next cycle")
			return
		}
		rec.ReplacementID = id
		app.closeReplacement(rec, replaceLate, reason)
	}
}

// completeScanWithText finishes a scan whose replacement paperless-ngx
// refused after the cycle that uploaded it: it gets the recognized text and
// leaves the queue for the LLM stage, as after a failed upload in version mode.
func (app *App) completeScanWithText(ctx context.Context, rec *OCRReplacement, reason string, logger *logrus.Entry) {
	scan, err := app.Client.GetDocument(ctx, rec.OriginalID)
	if err == nil {
		err = app.Client.UpdateDocuments(ctx, []DocumentSuggestion{app.autoOCRCompletion(scan, rec.Text)}, app.Database, false)
	}
	if err != nil {
		var partial *PartialUpdateError
		if errors.As(err, &partial) {
			applyFailTagAfterPartialSuccess(ctx, app.Client, app.Database, partial.DocumentID, partial.DroppedFields)
		} else {
			logger.WithError(err).Error("Could not complete the scan after its replacement was refused")
			rec.TaskID = "" // nothing more to watch: paperless-ngx refused it
			app.giveUpReplacement(ctx, rec, logger, reason+"; writing the text to the scan failed too")
			return
		}
	}
	if err := FinishOCRRun(app.Database, rec.JobID, "completed", "", 0, 0, "failed", "Not replaced: "+reason+"."); err != nil {
		logger.WithError(err).Warn("Could not update the OCR run")
	}
}

// autoOCRCompletion is the update that finishes an auto-OCR run on a document:
// the recognized text as content, the queue tag off, the OCR complete tag on.
func (app *App) autoOCRCompletion(document Document, text string) DocumentSuggestion {
	s := DocumentSuggestion{
		ID:               document.ID,
		OriginalDocument: document,
		SuggestedContent: text,
		RemoveTags:       []string{autoOcrTag},
	}
	if app.pdfOCRTagging && app.pdfOCRCompleteTag != "" {
		s.SuggestedTags = []string{app.pdfOCRCompleteTag}
		s.KeepOriginalTags = true
	}
	return s
}
