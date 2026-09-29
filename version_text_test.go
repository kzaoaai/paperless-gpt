package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedPatch is one PATCH of a document: its query (e.g. "version=3101")
// and body.
type recordedPatch struct {
	query string
	body  map[string]interface{}
}

// versionTextEnv mocks paperless-ngx for an OCR run that adds a searchable PDF
// as a new version: GET returns current as the document's (latest version's)
// content, task is the import's status, and every PATCH is recorded.
type versionTextEnv struct {
	*testEnv
	current   string
	task      string
	patchCode int
	patches   []recordedPatch
}

func newVersionTextEnv(t *testing.T, documentID int, current string) *versionTextEnv {
	t.Helper()
	e := &versionTextEnv{testEnv: newTestEnv(t), current: current, task: "success", patchCode: http.StatusOK}
	t.Cleanup(e.teardown)
	require.NoError(t, e.db.AutoMigrate(&OCRPageResult{}, &OCRRun{}))
	// The test database is shared across the package's tests.
	require.NoError(t, e.db.Where("document_id = ?", documentID).Delete(&ModificationHistory{}).Error)
	shortenTaskPolling(t)

	original, err := os.ReadFile("tests/pdf/sample.pdf")
	require.NoError(t, err)
	e.client.CacheFolder = t.TempDir()

	e.setMockResponse("/api/tags/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"results":[{"id":171,"name":"Inbox"},{"id":166,"name":"Identification"}]}`))
	})
	e.setMockResponse(fmt.Sprintf("/api/documents/%d/download/", documentID), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(original)
	})
	e.setMockResponse(fmt.Sprintf("/api/documents/%d/update_version/", documentID), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"task-text"`))
	})
	e.setMockResponse("/api/tasks/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"count":1,"results":[{"task_id":"task-text","status":%q}]}`, e.task)
	})
	e.setMockResponse(fmt.Sprintf("/api/documents/%d/", documentID), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			var body map[string]interface{}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			e.patches = append(e.patches, recordedPatch{query: r.URL.RawQuery, body: body})
			w.WriteHeader(e.patchCode)
			w.Write([]byte(`{}`))
			return
		}
		// Tags 900 and 901 are not visible to the API user, so GetDocument
		// cannot name them.
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(GetDocumentApiResponse{
			ID: documentID, Title: "Registry extract", Content: e.current, Tags: []int{166, 171, 900, 901},
		})
	})
	return e
}

// runManualVersionJob runs a manual OCR job that adds a version, the way the
// OCR page submits one, and returns the job and its persisted run.
func runManualVersionJob(t *testing.T, e *versionTextEnv, documentID int, word string) (*Job, OCRRun) {
	t.Helper()
	app := &App{Client: e.client, Database: e.db, ocrProvider: &hocrStubProvider{word: word}, ocrProcessMode: "pdf"}
	job := &Job{
		ID:         generateJobID(),
		DocumentID: documentID,
		Status:     "pending",
		Options:    OCROptions{UploadPDF: true, UploadMode: PDFUploadModeVersion, ProcessMode: "pdf", PromptOverride: "OCR"},
	}
	jobStore.addJob(job)
	require.NoError(t, CreateOCRRun(e.db, &OCRRun{JobID: job.ID, DocumentID: documentID, Trigger: "manual"}))

	processJob(app, job)

	stored, ok := jobStore.getJob(job.ID)
	require.True(t, ok)
	var run OCRRun
	require.NoError(t, e.db.Where("job_id = ?", job.ID).First(&run).Error)
	return stored, run
}

func contentModifications(t *testing.T, e *versionTextEnv, documentID int) []ModificationHistory {
	t.Helper()
	var mods []ModificationHistory
	require.NoError(t, e.db.Where("document_id = ? AND mod_field = ?", documentID, "content").Find(&mods).Error)
	return mods
}

func TestManualJobWritesRecognizedTextToNewVersionAndOriginal(t *testing.T) {
	const documentID = 3101
	e := newVersionTextEnv(t, documentID, "paperless-ngx's own extraction")

	job, run := runManualVersionJob(t, e, documentID, "الجمهورية")

	assert.Equal(t, "completed", job.Status)
	assert.Equal(t, "versioned", run.PDFAction)
	assert.Empty(t, run.PDFDetail)
	assert.Contains(t, job.Result, "الجمهورية")
	content := map[string]interface{}{"content": job.Result}
	assert.Equal(t, []recordedPatch{
		{query: "", body: content},             // the latest version: the new one
		{query: "version=3101", body: content}, // the original, which search indexes
	}, e.patches, "only content is written, so tags the API user cannot see survive")

	mods := contentModifications(t, e, documentID)
	require.Len(t, mods, 1, "the new version's change can be undone from the history")
	assert.Equal(t, "paperless-ngx's own extraction", mods[0].PreviousValue)
	assert.Equal(t, job.Result, mods[0].NewValue)
}

func TestManualJobWritesOnlyTheOriginalWhenTheVersionMatches(t *testing.T) {
	const documentID = 3102
	e := newVersionTextEnv(t, documentID, "الجمهورية")

	_, run := runManualVersionJob(t, e, documentID, "الجمهورية")

	assert.Equal(t, "versioned", run.PDFAction)
	assert.Equal(t, []recordedPatch{{query: "version=3102", body: map[string]interface{}{"content": "الجمهورية"}}}, e.patches)
	assert.Empty(t, contentModifications(t, e, documentID))
}

func TestManualJobWithUnconfirmedVersionWritesNoText(t *testing.T) {
	const documentID = 3103
	e := newVersionTextEnv(t, documentID, "paperless-ngx's own extraction")
	e.task = "started"

	_, run := runManualVersionJob(t, e, documentID, "الجمهورية")

	assert.Equal(t, "versioned", run.PDFAction)
	assert.Contains(t, run.PDFDetail, "had not confirmed the new version")
	assert.Contains(t, run.PDFDetail, "The recognized text was not written to it.")
	assert.Empty(t, e.patches, "the text could land on the document before its new version exists")
}

func TestManualJobWithRejectedVersionWritesNoText(t *testing.T) {
	const documentID = 3106
	e := newVersionTextEnv(t, documentID, "the original's content")
	e.task = "failure"

	_, run := runManualVersionJob(t, e, documentID, "الجمهورية")

	assert.Equal(t, "failed", run.PDFAction)
	assert.Empty(t, e.patches, "without a new version the text would overwrite the original's content")
}

func TestManualJobReportsFailedTextWrite(t *testing.T) {
	const documentID = 3104
	e := newVersionTextEnv(t, documentID, "paperless-ngx's own extraction")
	e.patchCode = http.StatusInternalServerError

	job, run := runManualVersionJob(t, e, documentID, "الجمهورية")

	assert.Equal(t, "completed", job.Status, "the version itself was added")
	assert.Equal(t, "versioned", run.PDFAction)
	assert.Contains(t, run.PDFDetail, "writing the recognized text to it failed")
	assert.Empty(t, contentModifications(t, e, documentID))
}

func TestSaveVersionTextIgnoresBlankText(t *testing.T) {
	const documentID = 3107
	e := newVersionTextEnv(t, documentID, "paperless-ngx's own extraction")
	app := &App{Client: e.client, Database: e.db}
	before := e.requestCount

	require.NoError(t, app.saveVersionText(context.Background(), documentID, " \n\n "))

	assert.Equal(t, before, e.requestCount, "no request for text that has nothing to write")
}

// runAutoOCR runs the auto-OCR loop over one queued document with the given
// content from before OCR; the stub recognizes "Test content".
func runAutoOCR(t *testing.T, e *versionTextEnv, documentID int, before string) {
	t.Helper()
	restoreTags := []string{autoTag, autoOcrTag, pdfOCRCompleteTag}
	autoTag, autoOcrTag, pdfOCRCompleteTag = "paperless-gpt-auto", "paperless-gpt-ocr-auto", "paperless-gpt-ocr-complete"
	restoreTemplate := ocrTemplate
	ocrTemplate = template.Must(template.New("ocr").Parse("OCR"))
	t.Cleanup(func() {
		autoTag, autoOcrTag, pdfOCRCompleteTag = restoreTags[0], restoreTags[1], restoreTags[2]
		ocrTemplate = restoreTemplate
	})

	setupTestCase(&OCRTestCase{documents: []TestDocument{{ID: documentID, Title: "Registry extract", Tags: []string{autoOcrTag}}}}, e.testEnv)
	e.setMockResponse("/api/tags/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("name__iexact") != "" { // the queue's size
			fmt.Fprintf(w, `{"count":1,"results":[{"id":2,"name":%q,"document_count":1}]}`, autoOcrTag)
			return
		}
		fmt.Fprintf(w, `{"results":[{"id":1,"name":%q},{"id":2,"name":%q},{"id":4,"name":%q}]}`, autoTag, autoOcrTag, pdfOCRCompleteTag)
	})
	e.setMockResponse("/api/documents/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(GetDocumentsApiResponse{
			Count: 1,
			Results: []GetDocumentApiResponseResult{
				{ID: documentID, Title: "Registry extract", Tags: []int{2}, Content: before, CreatedDate: "2026-09-01"},
			},
		})
	})

	app := &App{
		Client: e.client, Database: e.db, ocrProvider: &hocrStubProvider{word: "Test content"},
		ocrProcessMode: "pdf", pdfUpload: true, pdfUploadMode: PDFUploadModeVersion,
	}
	processed, err := app.processAutoOcrTagDocuments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
}

// contentWrites returns, per query, the content each PATCH wrote.
func contentWrites(patches []recordedPatch) map[string][]interface{} {
	writes := map[string][]interface{}{}
	for _, patch := range patches {
		if content, ok := patch.body["content"]; ok {
			writes[patch.query] = append(writes[patch.query], content)
		}
	}
	return writes
}

// TestAutoOCRWritesTextEqualToTheOldContent covers a document OCR'd again: its
// content from before OCR already equals the recognized text, but the new
// version holds paperless-ngx's extraction, which the text must replace.
func TestAutoOCRWritesTextEqualToTheOldContent(t *testing.T) {
	const documentID = 3105
	e := newVersionTextEnv(t, documentID, "paperless-ngx's own extraction")

	runAutoOCR(t, e, documentID, "Test content")

	assert.Equal(t, map[string][]interface{}{
		"":             {"Test content"},
		"version=3105": {"Test content"},
	}, contentWrites(e.patches), "written once to the new version and once to the original")
}

// TestAutoOCRWritesTheTextOnce covers a new scan: its content from before OCR
// is paperless-ngx's own OCR, which the tag update must not write over the
// recognized text again, nor record as a second change.
func TestAutoOCRWritesTheTextOnce(t *testing.T) {
	const documentID = 3109
	e := newVersionTextEnv(t, documentID, "paperless-ngx's own extraction")

	runAutoOCR(t, e, documentID, "paperless-ngx's own OCR of the scan")

	assert.Equal(t, map[string][]interface{}{
		"":             {"Test content"},
		"version=3109": {"Test content"},
	}, contentWrites(e.patches))
	assert.Len(t, contentModifications(t, e, documentID), 1)
}

func TestAutoOCRWithRejectedVersionLeavesTheOriginalAlone(t *testing.T) {
	const documentID = 3108
	e := newVersionTextEnv(t, documentID, "the original's content")
	e.task = "failure"

	runAutoOCR(t, e, documentID, "Test content")

	assert.NotContains(t, contentWrites(e.patches), "version=3108")
}
