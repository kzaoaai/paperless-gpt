package main

// One real auto-OCR cycle in replace mode against a running paperless-ngx,
// with Document AI stubbed (countingProvider recognizes REPLACE_E2E_WORD on
// every page). Skipped unless REPLACE_E2E_URL is set. State lives in the
// sqlite file REPLACE_E2E_DB, so consecutive runs continue one another, as
// consecutive cycles do. A throwaway paperless-ngx only: the cycle uploads,
// updates and deletes documents.
//
//	REPLACE_E2E_URL=http://127.0.0.1:8010 REPLACE_E2E_TOKEN=... \
//	REPLACE_E2E_DB=/tmp/e2e.sqlite REPLACE_E2E_WORD=Receipt \
//	go test -run TestReplaceE2ECycle -count=1 -v .

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestReplaceE2ECycle(t *testing.T) {
	base := os.Getenv("REPLACE_E2E_URL")
	if base == "" {
		t.Skip("REPLACE_E2E_URL not set")
	}
	db, err := gorm.Open(sqlite.Open(os.Getenv("REPLACE_E2E_DB")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&ModificationHistory{}, &OCRPageResult{}, &OCRRun{}, &ProcessingFailure{}))

	saved := []string{autoTag, autoOcrTag, pdfOCRCompleteTag, failTag}
	savedTemplate, savedReplace, savedLimit := ocrTemplate, autoOCRReplace, limitOcrPages
	attempts, interval := taskPollAttempts, taskPollInterval
	t.Cleanup(func() {
		autoTag, autoOcrTag, pdfOCRCompleteTag, failTag = saved[0], saved[1], saved[2], saved[3]
		ocrTemplate, autoOCRReplace, limitOcrPages = savedTemplate, savedReplace, savedLimit
		taskPollAttempts, taskPollInterval = attempts, interval
	})
	autoTag, autoOcrTag, pdfOCRCompleteTag, failTag = "OCR-auto-done", "Inbox", "OCR-auto-done", "paperless-gpt-failed"
	ocrTemplate = template.Must(template.New("ocr").Parse("OCR"))
	autoOCRReplace, limitOcrPages = true, 30
	taskPollAttempts, taskPollInterval = 60, time.Second
	if n, err := strconv.Atoi(os.Getenv("REPLACE_E2E_POLLS")); err == nil {
		taskPollAttempts = n
	}

	client := NewPaperlessClient(base, os.Getenv("REPLACE_E2E_TOKEN"))
	client.CacheFolder = t.TempDir()
	provider := &countingProvider{hocrStubProvider: hocrStubProvider{word: os.Getenv("REPLACE_E2E_WORD")}}
	app := &App{
		Client: client, Database: db, ocrProvider: provider, ocrProcessMode: "pdf",
		pdfUpload: true, pdfUploadMode: PDFUploadModeVersion, pdfSkipExistingOCR: true,
		pdfOCRTagging: true, pdfOCRCompleteTag: "OCR-auto-done",
	}
	n, err := app.processAutoOcrTagDocuments(context.Background())

	var recs []OCRReplacement
	db.Find(&recs)
	var runs []OCRRun
	db.Order("id").Find(&runs)
	type runOut struct {
		Doc               int
		Mode, Action, Det string
	}
	var outRuns []runOut
	for _, r := range runs {
		outRuns = append(outRuns, runOut{r.DocumentID, r.UploadMode, r.PDFAction, r.PDFDetail})
	}
	out, _ := json.MarshalIndent(map[string]interface{}{
		"processed": n, "error": errString(err), "document_ai_calls": provider.calls,
		"records": recs, "runs": outRuns,
	}, "", "  ")
	t.Log(string(out))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
