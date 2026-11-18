package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/template"
	"time"

	"paperless-gpt/ocr"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Tag ids in the fake, named as in a setup where the OCR complete tag is also
// the LLM stage's trigger (AUTO_TAG), as in the fork's production config.
const (
	fakeInbox      = 171 // AUTO_OCR_TAG, also paperless-ngx's inbox tag
	fakeOCRDone    = 577 // PDF_OCR_COMPLETE_TAG = AUTO_TAG
	fakeFailTag    = 581
	fakeMatchedTag = 166 // matched on the scan at consume
	fakeOtherMatch = 167 // matched only on the replacement at its consume
	fakeAPIUser    = 2
	fakeViewer     = 6
)

type fakeDoc struct {
	ID            int
	Title         string
	Content       string
	Tags          []int
	Correspondent interface{}
	DocumentType  interface{}
	StoragePath   interface{}
	Created       string
	Added         time.Time
	ASN           interface{}
	FileName      string
	Owner         interface{}
	Perms         map[string]interface{}
	Notes         []interface{}
	CustomFields  []interface{}
	Versions      int // versions besides the root
	Deleted       bool
	File          []byte
}

type replaceTask struct {
	id     string
	docID  int
	upload fakeUpload
}

type fakeUpload struct {
	filename string
	title    string
	created  string
	data     []byte
}

// replaceFake is a stateful paperless-ngx 3.4 for replace mode: documents with
// owners, permissions and a trash, consume tasks in 3.4's shape, and a consume
// that, like production's workflows 5 and 6, puts every new PDF in the OCR
// queue and assigns what its own matching finds.
type replaceFake struct {
	t      *testing.T
	mu     sync.Mutex
	server *httptest.Server
	docs   map[int]*fakeDoc
	nextID int
	tasks  map[string]*replaceTask
	scan   []byte

	consume        string // "success" (default), "failure" or "pending"
	patchFailures  map[int]int
	deleteFailures map[int]int
	links          map[int]int

	uploads        []fakeUpload
	versionUploads int
	deletes        []int
	events         []string // "patch:<id>", "delete:<id>", "consumed:<id>" in order
	patches        map[int][]map[string]interface{}
}

func newReplaceFake(t *testing.T) *replaceFake {
	scan, err := os.ReadFile("tests/pdf/sample.pdf")
	require.NoError(t, err)
	f := &replaceFake{
		t: t, docs: map[int]*fakeDoc{}, nextID: 500, tasks: map[string]*replaceTask{}, scan: scan,
		consume: "success", patchFailures: map[int]int{}, deleteFailures: map[int]int{}, links: map[int]int{},
		patches: map[int][]map[string]interface{}{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// addScan adds a scan the way a consume-folder upload arrives: no owner, the
// viewer's grant from workflow 8, in the OCR queue, matched by paperless-ngx.
func (f *replaceFake) addScan(id int) *fakeDoc {
	d := &fakeDoc{
		ID: id, Title: "2026-11-18 9.15.39 PM", Content: "tesseract's reading", Tags: []int{fakeInbox, fakeMatchedTag},
		Correspondent: float64(315), DocumentType: float64(9), Created: "2026-11-18", Added: time.Now().Add(-time.Minute),
		FileName: "2026-11-18 9.15.39 PM.pdf", Perms: perms(fakeViewer), File: f.scan,
	}
	f.docs[id] = d
	return d
}

func perms(viewUsers ...int) map[string]interface{} {
	users := []interface{}{}
	for _, u := range viewUsers {
		users = append(users, float64(u))
	}
	return map[string]interface{}{
		"view":   map[string]interface{}{"users": users, "groups": []interface{}{}},
		"change": map[string]interface{}{"users": []interface{}{}, "groups": []interface{}{}},
	}
}

func (f *replaceFake) tagName(id int) string {
	switch id {
	case fakeInbox:
		return "Inbox"
	case fakeOCRDone:
		return "OCR-auto-done"
	case fakeFailTag:
		return "paperless-gpt-failed"
	case fakeMatchedTag:
		return "Education"
	case fakeOtherMatch:
		return "Tuition"
	}
	return ""
}

func (f *replaceFake) json(d *fakeDoc) map[string]interface{} {
	versions := []interface{}{map[string]interface{}{"id": d.ID, "is_root": true}}
	for i := 0; i < d.Versions; i++ {
		versions = append(versions, map[string]interface{}{"id": d.ID + 1000 + i, "is_root": false})
	}
	return map[string]interface{}{
		"id": d.ID, "title": d.Title, "content": d.Content, "tags": d.Tags,
		"correspondent": d.Correspondent, "document_type": d.DocumentType, "storage_path": d.StoragePath,
		"created": d.Created, "created_date": d.Created, "added": d.Added.Format(time.RFC3339Nano),
		"archive_serial_number": d.ASN, "original_file_name": d.FileName, "owner": d.Owner,
		"permissions": d.Perms, "notes": nonNil(d.Notes), "custom_fields": nonNil(d.CustomFields),
		"versions": versions, "root_document": nil, "mime_type": "application/pdf",
	}
}

func nonNil(v []interface{}) []interface{} {
	if v == nil {
		return []interface{}{}
	}
	return v
}

func (f *replaceFake) live(id int) (*fakeDoc, bool) {
	d, ok := f.docs[id]
	return d, ok && !d.Deleted
}

func replyJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (f *replaceFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	q := r.URL.Query()
	switch {
	case p == "/api/tags/" && r.Method == http.MethodGet:
		if name := q.Get("name__iexact"); name != "" {
			var id, n int
			for _, tid := range []int{fakeInbox, fakeOCRDone, fakeFailTag, fakeMatchedTag, fakeOtherMatch} {
				if strings.EqualFold(f.tagName(tid), name) {
					id = tid
				}
			}
			for _, d := range f.docs {
				if !d.Deleted && slices.Contains(d.Tags, id) {
					n++
				}
			}
			replyJSON(w, 200, map[string]interface{}{"count": 1, "results": []interface{}{map[string]interface{}{"id": id, "name": name, "document_count": n}}})
			return
		}
		var results []interface{}
		for _, tid := range []int{fakeInbox, fakeOCRDone, fakeFailTag, fakeMatchedTag, fakeOtherMatch} {
			results = append(results, map[string]interface{}{"id": tid, "name": f.tagName(tid)})
		}
		replyJSON(w, 200, map[string]interface{}{"count": len(results), "results": results})
	case p == "/api/correspondents/", p == "/api/document_types/", p == "/api/users/":
		replyJSON(w, 200, map[string]interface{}{"count": 0, "results": []interface{}{}})
	case p == "/api/custom_fields/":
		replyJSON(w, 200, map[string]interface{}{"count": 1, "results": []interface{}{map[string]interface{}{"id": 23, "name": "Related", "data_type": "documentlink"}}})
	case p == "/api/documents/" && r.Method == http.MethodGet:
		if cfq := q.Get("custom_field_query"); cfq != "" {
			var parsed []interface{}
			require.NoError(f.t, json.Unmarshal([]byte(cfq), &parsed))
			require.Equal(f.t, "contains", parsed[1])
			target := int(parsed[2].([]interface{})[0].(float64))
			replyJSON(w, 200, map[string]interface{}{"count": f.links[target], "results": []interface{}{}})
			return
		}
		name := q.Get("tags__name__iexact")
		var results []interface{}
		ids := make([]int, 0, len(f.docs))
		for id := range f.docs {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			d := f.docs[id]
			if d.Deleted {
				continue
			}
			for _, tid := range d.Tags {
				if strings.EqualFold(f.tagName(tid), name) {
					results = append(results, f.json(d))
				}
			}
		}
		replyJSON(w, 200, map[string]interface{}{"count": len(results), "results": results})
	case p == "/api/documents/post_document/":
		require.NoError(f.t, r.ParseMultipartForm(32<<20))
		file, header, err := r.FormFile("document")
		require.NoError(f.t, err)
		data, _ := io.ReadAll(file)
		up := fakeUpload{filename: header.Filename, title: r.FormValue("title"), created: r.FormValue("created"), data: data}
		f.uploads = append(f.uploads, up)
		id := fmt.Sprintf("task-%d", len(f.tasks)+1)
		f.tasks[id] = &replaceTask{id: id, upload: up}
		replyJSON(w, 200, id)
	case strings.HasSuffix(p, "/update_version/"):
		f.versionUploads++
		replyJSON(w, 200, "task-version")
	case p == "/api/tasks/":
		id := q.Get("task_id")
		if id == "task-version" {
			replyJSON(w, 200, map[string]interface{}{"count": 1, "results": []interface{}{map[string]interface{}{"task_id": id, "status": "success"}}})
			return
		}
		task, ok := f.tasks[id]
		if !ok {
			replyJSON(w, 200, map[string]interface{}{"count": 0, "results": []interface{}{}})
			return
		}
		res := map[string]interface{}{"task_id": id, "task_type": "consume_file"}
		switch f.consume {
		case "failure":
			res["status"] = "failure"
			res["result_data"] = map[string]interface{}{"error": "Not consuming: it is a duplicate"}
		case "pending":
			res["status"] = "started"
		default:
			if task.docID == 0 {
				task.docID = f.consumeUpload(task.upload)
			}
			res["status"] = "success"
			res["result_data"] = map[string]interface{}{"document_id": task.docID}
			res["related_document_ids"] = []int{task.docID}
		}
		replyJSON(w, 200, map[string]interface{}{"count": 1, "results": []interface{}{res}})
	case strings.HasPrefix(p, "/api/documents/"):
		rest := strings.Trim(strings.TrimPrefix(p, "/api/documents/"), "/")
		parts := strings.Split(rest, "/")
		id, err := strconv.Atoi(parts[0])
		require.NoError(f.t, err, p)
		d, ok := f.live(id)
		if !ok {
			replyJSON(w, 404, map[string]string{"detail": "No Document matches the given query."})
			return
		}
		if len(parts) == 2 && parts[1] == "download" {
			w.WriteHeader(200)
			w.Write(d.File)
			return
		}
		switch r.Method {
		case http.MethodGet:
			replyJSON(w, 200, f.json(d))
		case http.MethodPatch:
			if f.patchFailures[id] > 0 {
				f.patchFailures[id]--
				replyJSON(w, 500, map[string]string{"detail": "server error"})
				return
			}
			var body map[string]interface{}
			require.NoError(f.t, json.NewDecoder(r.Body).Decode(&body))
			f.patches[id] = append(f.patches[id], body)
			f.events = append(f.events, fmt.Sprintf("patch:%d", id))
			f.applyPatch(d, body)
			replyJSON(w, 200, f.json(d))
		case http.MethodDelete:
			if f.deleteFailures[id] > 0 {
				f.deleteFailures[id]--
				replyJSON(w, 500, map[string]string{"detail": "server error"})
				return
			}
			d.Deleted = true
			f.deletes = append(f.deletes, id)
			f.events = append(f.events, fmt.Sprintf("delete:%d", id))
			w.WriteHeader(204)
		}
	default:
		f.t.Fatalf("unexpected request %s %s", r.Method, r.URL)
	}
}

// consumeUpload creates the document an upload becomes: owned by the API user
// (the uploader), no grants, the OCR queue tag from workflows 5 and 6, its own
// matching's correspondent and tag, and paperless-ngx's reading of the layer.
func (f *replaceFake) consumeUpload(up fakeUpload) int {
	f.nextID++
	d := &fakeDoc{
		ID: f.nextID, Title: up.title, Content: "pdftotext's reading of the layer", Tags: []int{fakeInbox, fakeOtherMatch},
		Correspondent: float64(77), DocumentType: nil, Created: up.created, Added: time.Now(), FileName: up.filename,
		Owner: float64(fakeAPIUser), Perms: perms(), File: up.data,
	}
	if d.Title == "" {
		d.Title = strings.TrimSuffix(up.filename, ".pdf")
	}
	f.docs[d.ID] = d
	f.events = append(f.events, fmt.Sprintf("consumed:%d", d.ID))
	return d.ID
}

func (f *replaceFake) applyPatch(d *fakeDoc, body map[string]interface{}) {
	for k, v := range body {
		switch k {
		case "content":
			d.Content = strings.TrimSpace(v.(string)) // DRF trims
		case "title":
			d.Title = v.(string)
		case "tags":
			d.Tags = nil
			for _, t := range v.([]interface{}) {
				d.Tags = append(d.Tags, int(t.(float64)))
			}
		case "correspondent":
			d.Correspondent = v
		case "document_type":
			d.DocumentType = v
		case "storage_path":
			d.StoragePath = v
		case "created", "created_date":
			d.Created = v.(string)
		case "owner":
			d.Owner = v
		case "set_permissions":
			d.Perms = v.(map[string]interface{})
		case "custom_fields":
			d.CustomFields = v.([]interface{})
		default:
			f.t.Fatalf("unexpected PATCH field %q", k)
		}
	}
}

// countingProvider is hocrStubProvider that counts Document AI calls.
type countingProvider struct {
	hocrStubProvider
	calls int
}

func (p *countingProvider) ProcessImage(ctx context.Context, data []byte, page int) (*ocr.OCRResult, error) {
	p.calls++
	return p.hocrStubProvider.ProcessImage(ctx, data, page)
}

func (p *countingProvider) ResetHOCR() { p.pages = nil }

type replaceEnv struct {
	fake     *replaceFake
	app      *App
	provider *countingProvider
	db       *gorm.DB
}

// newReplaceEnv sets up the auto-OCR loop the way production runs it, with
// replace mode on.
func newReplaceEnv(t *testing.T) *replaceEnv {
	t.Helper()
	fake := newReplaceFake(t)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "db.sqlite")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ModificationHistory{}, &OCRPageResult{}, &OCRRun{}, &ProcessingFailure{}))

	saved := []string{autoTag, autoOcrTag, pdfOCRCompleteTag, failTag}
	savedTemplate := ocrTemplate
	savedReplace, savedAge := autoOCRReplace, autoOCRReplaceMaxAge
	savedLimit := limitOcrPages
	autoTag, autoOcrTag, pdfOCRCompleteTag, failTag = "OCR-auto-done", "Inbox", "OCR-auto-done", "paperless-gpt-failed"
	ocrTemplate = template.Must(template.New("ocr").Parse("OCR"))
	autoOCRReplace, autoOCRReplaceMaxAge = true, 7*24*time.Hour
	limitOcrPages = 30
	shortenTaskPolling(t)
	t.Cleanup(func() {
		autoTag, autoOcrTag, pdfOCRCompleteTag, failTag = saved[0], saved[1], saved[2], saved[3]
		ocrTemplate = savedTemplate
		autoOCRReplace, autoOCRReplaceMaxAge = savedReplace, savedAge
		limitOcrPages = savedLimit
	})

	client := NewPaperlessClient(fake.server.URL, "test-token")
	client.HTTPClient = fake.server.Client()
	client.CacheFolder = t.TempDir()
	provider := &countingProvider{hocrStubProvider: hocrStubProvider{word: "Receipt"}}
	app := &App{
		Client: client, Database: db, ocrProvider: provider, ocrProcessMode: "pdf",
		pdfUpload: true, pdfUploadMode: PDFUploadModeVersion,
		pdfOCRTagging: true, pdfOCRCompleteTag: "OCR-auto-done",
	}
	return &replaceEnv{fake: fake, app: app, provider: provider, db: db}
}

func (e *replaceEnv) cycle(t *testing.T) {
	t.Helper()
	_, err := e.app.processAutoOcrTagDocuments(context.Background())
	require.NoError(t, err)
}

func (e *replaceEnv) run(t *testing.T, documentID int) OCRRun {
	t.Helper()
	var run OCRRun
	require.NoError(t, e.db.Where("document_id = ?", documentID).Order("id desc").First(&run).Error)
	return run
}

func (e *replaceEnv) record(t *testing.T, documentID int) OCRReplacement {
	t.Helper()
	var rec OCRReplacement
	require.NoError(t, e.db.Where("original_id = ?", documentID).Order("id desc").First(&rec).Error)
	return rec
}

func (e *replaceEnv) replacement(t *testing.T) *fakeDoc {
	t.Helper()
	for _, d := range e.fake.docs {
		if d.ID > 500 && !d.Deleted {
			return d
		}
	}
	t.Fatal("no replacement document")
	return nil
}

// Behaviours 1–5: a new scan is OCR'd once, its searchable PDF is consumed as
// a new document, which gets the scan's metadata, permissions and the
// recognized text; only then is the scan deleted, and only the replacement is
// queued for the LLM stage.
func TestReplaceNewScan(t *testing.T) {
	e := newReplaceEnv(t)
	scan := e.fake.addScan(100)
	scan.StoragePath = float64(4)

	e.cycle(t)

	require.Len(t, e.fake.uploads, 1, "one upload as a new document")
	assert.Equal(t, 0, e.fake.versionUploads, "no version")
	up := e.fake.uploads[0]
	assert.Equal(t, "2026-11-18 9.15.39 PM.pdf", up.filename, "the replacement keeps the scan's file name")
	assert.True(t, hasSearchableLayer(up.data), "the upload is the searchable PDF")

	r := e.replacement(t)
	assert.Equal(t, "Receipt", r.Content, "content is the recognized text, not paperless-ngx's reading of the layer")
	assert.ElementsMatch(t, []int{fakeMatchedTag, fakeOCRDone}, r.Tags, "the scan's tags, queue tag swapped for the OCR complete tag (= AUTO_TAG)")
	assert.Nil(t, r.Owner, "the scan's owner (none), not the uploading API user")
	assert.Equal(t, perms(fakeViewer), r.Perms, "the scan's permissions, the viewer's grant included")
	assert.Equal(t, float64(315), r.Correspondent, "the scan's correspondent, not the replacement's own match")
	assert.Equal(t, float64(9), r.DocumentType)
	assert.Equal(t, float64(4), r.StoragePath)
	assert.Equal(t, "2026-11-18 9.15.39 PM", r.Title)
	assert.Equal(t, "2026-11-18", r.Created)

	assert.True(t, e.fake.docs[100].Deleted, "the scan is in the trash")
	assert.Equal(t, []string{fmt.Sprintf("consumed:%d", r.ID), fmt.Sprintf("patch:%d", r.ID), "delete:100"}, e.fake.events,
		"deleted only after the consume was confirmed and the hand-over was written; the scan itself is never updated")
	assert.Empty(t, e.fake.patches[100], "the scan never gets the LLM trigger tag")

	run := e.run(t, 100)
	assert.Equal(t, "replaced", run.PDFAction)
	assert.Contains(t, run.PDFDetail, fmt.Sprintf("document %d", r.ID))
	assert.Equal(t, PDFUploadModeReplace, run.UploadMode)
	rec := e.record(t, 100)
	assert.Equal(t, replaceDone, rec.State)
	assert.Equal(t, r.ID, rec.ReplacementID)
	assert.Empty(t, rec.Text, "the text is not kept once handed over")
	var mods []ModificationHistory
	require.NoError(t, e.db.Where("document_id = ? AND mod_field = ?", r.ID, "content").Find(&mods).Error)
	assert.Len(t, mods, 1, "the content change is in the history, for undo")
	assert.Equal(t, 1, e.provider.calls, "one Document AI call")

	// The next cycles: nothing left in the OCR queue, no second call or upload.
	e.cycle(t)
	e.cycle(t)
	assert.Equal(t, 1, e.provider.calls)
	assert.Len(t, e.fake.uploads, 1)
}

// Behaviour 4: a replacement that is back in the queue (re-tagged by
// workflows 5 and 6 at its consume, and its hand-over not done yet) is never
// sent through OCR again.
func TestReplaceHandOverFailureKeepsTheScanAndResumes(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.fake.patchFailures[501] = 1

	e.cycle(t)

	assert.False(t, e.fake.docs[100].Deleted, "the scan stays while the hand-over is not done")
	assert.Contains(t, e.fake.docs[100].Tags, fakeInbox, "and stays in the queue, untouched")
	assert.Empty(t, e.fake.patches[100])
	r := e.fake.docs[501]
	assert.Contains(t, r.Tags, fakeInbox, "the replacement is in the OCR queue too")
	assert.Equal(t, "attached", e.run(t, 100).PDFAction)
	assert.Equal(t, replaceConsumed, e.record(t, 100).State)

	e.cycle(t)

	assert.Equal(t, 1, e.provider.calls, "neither the scan nor its replacement is OCR'd again")
	assert.Len(t, e.fake.uploads, 1, "nor uploaded again")
	assert.True(t, e.fake.docs[100].Deleted)
	assert.ElementsMatch(t, []int{fakeMatchedTag, fakeOCRDone}, r.Tags)
	assert.Equal(t, "Receipt", r.Content)
	assert.Equal(t, "replaced", e.run(t, 100).PDFAction, "the run is updated once the replacement is done")
	assert.Equal(t, replaceDone, e.record(t, 100).State)
}

// Behaviour 2: a consume paperless-ngx did not confirm in time leaves the scan
// in place and untouched; a later cycle finishes it without a second upload
// or OCR call.
func TestReplaceUnconfirmedConsumeKeepsTheScan(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.fake.consume = "pending"

	e.cycle(t)

	assert.Empty(t, e.fake.deletes)
	assert.Empty(t, e.fake.patches[100], "the scan is not touched")
	assert.Contains(t, e.fake.docs[100].Tags, fakeInbox)
	run := e.run(t, 100)
	assert.Equal(t, "attached", run.PDFAction)
	assert.Contains(t, run.PDFDetail, "had not confirmed")
	assert.Equal(t, replaceUploaded, e.record(t, 100).State)

	e.cycle(t) // still pending: nothing happens
	assert.Empty(t, e.fake.deletes)
	assert.Equal(t, 1, e.provider.calls)

	e.fake.consume = "success"
	e.cycle(t)

	assert.Equal(t, 1, e.provider.calls)
	assert.Len(t, e.fake.uploads, 1)
	assert.Equal(t, []int{100}, e.fake.deletes)
	assert.Equal(t, "Receipt", e.replacement(t).Content)
	assert.Equal(t, "replaced", e.run(t, 100).PDFAction)
}

// Behaviour 2: an upload never confirmed within the limit marks the scan
// failed and deletes nothing; a replacement arriving later is marked too.
func TestReplaceNeverConfirmedMarksTheScanFailed(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.fake.consume = "pending"
	e.cycle(t)
	require.NoError(t, e.db.Model(&OCRReplacement{}).Where("original_id = ?", 100).Update("created_at", time.Now().Add(-2*replacePendingLimit)).Error)

	e.cycle(t)

	scan := e.fake.docs[100]
	assert.False(t, scan.Deleted)
	assert.NotContains(t, scan.Tags, fakeInbox, "out of the queue")
	assert.Contains(t, scan.Tags, fakeFailTag, "marked for a person")
	assert.NotContains(t, scan.Tags, fakeOCRDone, "and not sent to the LLM stage")
	assert.Equal(t, replaceGaveUp, e.record(t, 100).State)
	run := e.run(t, 100)
	assert.Equal(t, "failed", run.PDFAction)
	var failures []ProcessingFailure
	require.NoError(t, e.db.Where("document_id = ?", 100).Find(&failures).Error)
	require.Len(t, failures, 1)
	assert.Contains(t, failures[0].Reason, "did not confirm")

	e.fake.consume = "success"
	e.cycle(t)

	late := e.replacement(t)
	assert.NotContains(t, late.Tags, fakeInbox, "the late replacement is not OCR'd")
	assert.Contains(t, late.Tags, fakeFailTag, "it is marked, so both show up for a person")
	assert.Equal(t, 1, e.provider.calls)
	assert.Empty(t, e.fake.deletes)
	assert.Equal(t, replaceLate, e.record(t, 100).State)
}

// Behaviour 2 and 6: paperless-ngx refusing the replacement deletes nothing,
// and the scan goes on as in version mode.
func TestReplaceRefusedFallsBackToVersion(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.fake.consume = "failure"

	e.cycle(t)

	assert.Empty(t, e.fake.deletes)
	assert.Equal(t, 1, e.fake.versionUploads, "added as a version instead")
	scan := e.fake.docs[100]
	assert.ElementsMatch(t, []int{fakeMatchedTag, fakeOCRDone}, scan.Tags, "the scan leaves the queue for the LLM stage")
	assert.Equal(t, "Receipt", scan.Content)
	run := e.run(t, 100)
	assert.Equal(t, "versioned", run.PDFAction)
	assert.Contains(t, run.PDFDetail, "duplicate")
	assert.Equal(t, replaceRejected, e.record(t, 100).State)
}

// A refusal noticed in a later cycle (after the first gave up waiting)
// completes the scan with the recognized text it kept.
func TestReplaceRefusedLaterCompletesTheScan(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.fake.consume = "pending"
	e.cycle(t)
	e.fake.consume = "failure"

	e.cycle(t)

	scan := e.fake.docs[100]
	assert.False(t, scan.Deleted)
	assert.ElementsMatch(t, []int{fakeMatchedTag, fakeOCRDone}, scan.Tags)
	assert.Equal(t, "Receipt", scan.Content, "the text kept in the record")
	assert.Equal(t, "failed", e.run(t, 100).PDFAction)
	rec := e.record(t, 100)
	assert.Equal(t, replaceRejected, rec.State)
	assert.Empty(t, rec.Text)
	assert.Equal(t, 1, e.provider.calls)
}

func TestReplaceDeleteFailureIsRetried(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.fake.deleteFailures[100] = 1

	e.cycle(t)

	assert.False(t, e.fake.docs[100].Deleted)
	assert.Equal(t, replaceHandedOver, e.record(t, 100).State)
	r := e.replacement(t)
	assert.Contains(t, r.Tags, fakeOCRDone, "the replacement is already handed over")

	e.cycle(t)

	assert.True(t, e.fake.docs[100].Deleted)
	assert.Equal(t, replaceDone, e.record(t, 100).State)
	assert.Equal(t, 1, e.provider.calls)
	assert.Len(t, e.fake.uploads, 1)
}

// A record left "uploading" (paperless-gpt stopped mid-upload) is not
// uploaded again: the scan is marked for a person.
func TestReplaceInterruptedUploadMarksTheScan(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	require.NoError(t, ensureReplacementTable(e.db))
	require.NoError(t, e.db.Create(&OCRReplacement{OriginalID: 100, State: replaceUploading, Text: "Receipt"}).Error)

	e.cycle(t)

	assert.Empty(t, e.fake.uploads)
	assert.Equal(t, 0, e.provider.calls)
	assert.Contains(t, e.fake.docs[100].Tags, fakeFailTag)
	assert.NotContains(t, e.fake.docs[100].Tags, fakeInbox)
}

// Behaviour 6 and the guard: what is not a new scan keeps version mode.
func TestReplaceOnlyNewScans(t *testing.T) {
	cases := map[string]func(f *replaceFake, d *fakeDoc){
		"notes": func(_ *replaceFake, d *fakeDoc) { d.Notes = []interface{}{map[string]interface{}{"id": 1}} },
		"ASN":   func(_ *replaceFake, d *fakeDoc) { d.ASN = float64(12) },
		"custom fields": func(_ *replaceFake, d *fakeDoc) {
			d.CustomFields = []interface{}{map[string]interface{}{"field": 1, "value": "x"}}
		},
		"versions":        func(_ *replaceFake, d *fakeDoc) { d.Versions = 1 },
		"old":             func(_ *replaceFake, d *fakeDoc) { d.Added = time.Now().Add(-8 * 24 * time.Hour) },
		"linked":          func(f *replaceFake, d *fakeDoc) { f.links[d.ID] = 1 },
		"already layered": nil, // set below
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newReplaceEnv(t)
			d := e.fake.addScan(100)
			if mutate != nil {
				mutate(e.fake, d)
			} else {
				// A PDF that already carries this program's layer, read
				// differently this time (the same word would keep its text).
				d.File = searchablePDF(t)
				e.provider.word = "Invoice"
			}

			e.cycle(t)

			assert.Empty(t, e.fake.uploads, "no new document")
			assert.Equal(t, 1, e.fake.versionUploads, "a version, as before")
			assert.Empty(t, e.fake.deletes)
			assert.Equal(t, "versioned", e.run(t, 100).PDFAction)
		})
	}
}

// searchablePDF returns a searchable PDF this program made, by running replace
// mode once.
func searchablePDF(t *testing.T) []byte {
	t.Helper()
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.cycle(t)
	require.Len(t, e.fake.uploads, 1)
	return e.fake.uploads[0].data
}

// Behaviour 6: no searchable PDF, nothing replaced; the scan goes on as today.
func TestReplaceNothingWhenNoSearchablePDF(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	e.provider.word = "Receipt 😀" // the layer cannot encode it: the PDF is skipped

	e.cycle(t)

	assert.Empty(t, e.fake.uploads)
	assert.Equal(t, 0, e.fake.versionUploads)
	assert.Empty(t, e.fake.deletes)
	assert.ElementsMatch(t, []int{fakeMatchedTag, fakeOCRDone}, e.fake.docs[100].Tags)
	assert.Equal(t, "skipped", e.run(t, 100).PDFAction)
	var n int64
	e.db.Model(&OCRReplacement{}).Count(&n)
	assert.Zero(t, n)
}

// Replace mode off: the auto loop is version mode, as before.
func TestReplaceOffKeepsVersionMode(t *testing.T) {
	e := newReplaceEnv(t)
	autoOCRReplace = false
	e.fake.addScan(100)

	e.cycle(t)

	assert.Empty(t, e.fake.uploads)
	assert.Equal(t, 1, e.fake.versionUploads)
	assert.Empty(t, e.fake.deletes)
}

// Manual OCR jobs never replace: they keep the configured version mode.
func TestReplaceNeverInManualJobs(t *testing.T) {
	e := newReplaceEnv(t)
	e.fake.addScan(100)
	opts := e.app.effectiveOCRDefaults()
	assert.Equal(t, PDFUploadModeVersion, opts.UploadMode)

	job := &Job{ID: generateJobID(), DocumentID: 100, Status: "pending", Options: opts}
	jobStore.addJob(job)
	require.NoError(t, CreateOCRRun(e.db, &OCRRun{JobID: job.ID, DocumentID: 100, Trigger: "manual"}))
	processJob(e.app, job)

	assert.Empty(t, e.fake.uploads)
	assert.Equal(t, 1, e.fake.versionUploads)
	assert.Empty(t, e.fake.deletes)
}

func TestReplacementDocumentIDReadsEveryTaskShape(t *testing.T) {
	for name, task := range map[string]string{
		"3.4 result_data":      `{"status":"success","result_data":{"document_id":42}}`,
		"3.x related ids":      `{"status":"success","related_document_ids":[42]}`,
		"2.x related_document": `{"status":"SUCCESS","related_document":"42"}`,
	} {
		var m map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(task), &m))
		assert.Equal(t, 42, replacementDocumentID(m), name)
	}
	assert.Zero(t, replacementDocumentID(map[string]interface{}{"status": "success"}))
}

func TestLoadAutoOCRReplaceConfig(t *testing.T) {
	savedReplace, savedUpload, savedMode, savedPDFReplace, savedAge := autoOCRReplace, pdfUpload, pdfUploadMode, pdfReplace, autoOCRReplaceMaxAge
	t.Cleanup(func() {
		autoOCRReplace, pdfUpload, pdfUploadMode, pdfReplace, autoOCRReplaceMaxAge = savedReplace, savedUpload, savedMode, savedPDFReplace, savedAge
	})
	autoOCRReplace, pdfUpload, pdfUploadMode, pdfReplace = true, true, PDFUploadModeVersion, false
	t.Setenv("AUTO_OCR_REPLACE_MAX_AGE", "48h")
	require.NoError(t, loadAutoOCRReplaceConfig())
	assert.Equal(t, 48*time.Hour, autoOCRReplaceMaxAge)

	pdfUploadMode = PDFUploadModeNew
	assert.Error(t, loadAutoOCRReplaceConfig(), "needs version mode for everything else")
	pdfUploadMode, pdfUpload = PDFUploadModeVersion, false
	assert.Error(t, loadAutoOCRReplaceConfig(), "needs PDF_UPLOAD")
	t.Setenv("AUTO_OCR_REPLACE_MAX_AGE", "soon")
	assert.Error(t, loadAutoOCRReplaceConfig())
}
