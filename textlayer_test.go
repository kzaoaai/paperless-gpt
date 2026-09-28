package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/gardar/ocrchestra/pkg/hocr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUndrawableWordsCountsWordsTheFontLacks(t *testing.T) {
	doc := &hocr.HOCR{Pages: []hocr.Page{{
		Lines: []hocr.Line{{Words: []hocr.Word{{Text: "don’t"}, {Text: "2019–2020"}}}},
		Areas: []hocr.Area{{
			Paragraphs: []hocr.Paragraph{{Lines: []hocr.Line{{Words: []hocr.Word{{Text: "Café"}}}}}},
			Words:      []hocr.Word{{Text: "الجمهورية"}, {Text: "東京"}},
		}},
		Paragraphs: []hocr.Paragraph{{Words: []hocr.Word{{Text: "Привет"}}}},
	}}}

	undrawable, total := undrawableWords(doc)

	assert.Equal(t, 6, total)
	assert.Equal(t, 1, undrawable, "only the CJK word is missing from the font")
}

func TestUndrawableWordsNil(t *testing.T) {
	undrawable, total := undrawableWords(nil)
	assert.Zero(t, undrawable)
	assert.Zero(t, total)
}

// runVersionOCR runs ProcessDocumentOCR in version mode on tests/pdf/sample.pdf
// with a stub provider recognizing word, and reports what was uploaded.
func runVersionOCR(t *testing.T, word string) (*ProcessedDocument, []byte) {
	t.Helper()
	env := newTestEnv(t)
	t.Cleanup(env.teardown)
	require.NoError(t, env.db.AutoMigrate(&OCRPageResult{}))
	shortenTaskPolling(t)

	original, err := os.ReadFile("tests/pdf/sample.pdf")
	require.NoError(t, err)

	const documentID = 3001
	env.setMockResponse(fmt.Sprintf("/api/documents/%d/download/", documentID), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(original)
	})
	env.client.CacheFolder = t.TempDir()

	var uploaded []byte
	env.setMockResponse(fmt.Sprintf("/api/documents/%d/update_version/", documentID), func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(10<<20))
		file, _, err := r.FormFile("document")
		require.NoError(t, err)
		uploaded, _ = io.ReadAll(file)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`"task-layer"`))
	})
	env.setMockResponse("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"count":1,"results":[{"task_id":"task-layer","status":"success"}]}`))
	})

	app := &App{Client: env.client, Database: env.db, ocrProvider: &hocrStubProvider{word: word}, ocrProcessMode: "pdf"}
	options := OCROptions{UploadPDF: true, UploadMode: PDFUploadModeVersion, ProcessMode: "pdf", PromptOverride: "OCR"}
	doc, err := app.ProcessDocumentOCR(context.Background(), documentID, options, "")
	require.NoError(t, err)
	return doc, uploaded
}

func TestProcessDocumentOCRVersionsArabic(t *testing.T) {
	doc, uploaded := runVersionOCR(t, "الجمهورية")

	assert.Equal(t, "versioned", doc.PDFAction, doc.PDFDetail)
	require.NotEmpty(t, uploaded)
	assert.Contains(t, doc.Text, "الجمهورية")
}

func TestProcessDocumentOCRSkipsSearchablePDFForUndrawableText(t *testing.T) {
	doc, uploaded := runVersionOCR(t, "東京")

	assert.Equal(t, "skipped", doc.PDFAction)
	assert.Contains(t, doc.PDFDetail, "cannot draw")
	assert.Nil(t, uploaded, "no version is uploaded when the layer would lose text")
	assert.Contains(t, doc.Text, "東京", "the recognized text itself is untouched")
}

func TestProcessDocumentOCRUploadsTypographicPunctuation(t *testing.T) {
	doc, uploaded := runVersionOCR(t, "Invoice’s")

	assert.Equal(t, "versioned", doc.PDFAction, doc.PDFDetail)
	assert.NotEmpty(t, uploaded)
}
