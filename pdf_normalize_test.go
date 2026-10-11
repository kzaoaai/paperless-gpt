package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/ascii85"
	"encoding/binary"
	"fmt"
	"net/http"
	"testing"

	"github.com/gardar/ocrchestra/pkg/hocr"
	"github.com/gardar/ocrchestra/pkg/pdfocr"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pageContent draws a filled rectangle, so checkRebuiltPDF has ink to compare.
const pageContent = "0 0 0 rg 20 20 260 160 re f\n"

// pdfOf writes objects 1..n as a PDF with a classic xref table; object 1 is
// the catalog.
func pdfOf(objects ...string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

// iosAnnotatedPDF is a page with an empty FreeText annotation whose /DA is a
// name, as iOS form-field detection writes it.
func iosAnnotatedPDF() []byte {
	return pdfOf(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents 4 0 R /Annots [5 0 R] >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(pageContent), pageContent),
		"<< /Type /Annot /Subtype /FreeText /Rect [10 10 100 40] /F 4 /Q 0 /DA /Helvetica#2018#20Tf#200#20g >>",
	)
}

// ascii85PDF is a page whose content stream is ASCII85-encoded.
func ascii85PDF() []byte {
	enc := make([]byte, ascii85.MaxEncodedLen(len(pageContent)))
	enc = append(enc[:ascii85.Encode(enc, []byte(pageContent))], "~>"...)
	return pdfOf(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d /Filter /ASCII85Decode >>\nstream\n%s\nendstream", len(enc)+1, enc),
	)
}

func deflate(data []byte) []byte {
	var b bytes.Buffer
	z := zlib.NewWriter(&b)
	z.Write(data)
	z.Close()
	return b.Bytes()
}

// objectStreamPDF is a page whose content stream's /Length is an object kept
// in an object stream, with a cross-reference stream, as Ghostscript's
// pdfwrite writes it.
func objectStreamPDF() []byte {
	content := deflate([]byte(pageContent))
	objStm := deflate([]byte(fmt.Sprintf("5 0 %d", len(content))))

	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n")
	offsets := map[int]int{}
	write := func(n int, body string) {
		offsets[n] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", n, body)
	}
	write(1, "<< /Type /Catalog /Pages 2 0 R >>")
	write(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	write(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents 4 0 R >>")
	write(4, fmt.Sprintf("<< /Length 5 0 R /Filter /FlateDecode >>\nstream\n%s\nendstream", content))
	write(6, fmt.Sprintf("<< /Type /ObjStm /N 1 /First 4 /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream", len(objStm), objStm))

	// Rows of /W [1 4 1]: type, offset or object stream, generation or index.
	xrefAt := b.Len()
	var rows bytes.Buffer
	row := func(typ byte, field int, last byte) {
		rows.WriteByte(typ)
		binary.Write(&rows, binary.BigEndian, uint32(field))
		rows.WriteByte(last)
	}
	row(0, 0, 255)
	for n := 1; n <= 4; n++ {
		row(1, offsets[n], 0)
	}
	row(2, 6, 0) // object 5: first in object stream 6
	row(1, offsets[6], 0)
	row(1, xrefAt, 0)
	xrefData := deflate(rows.Bytes())
	fmt.Fprintf(&b, "7 0 obj\n<< /Type /XRef /Size 8 /W [1 4 1] /Root 1 0 R /Length %d /Filter /FlateDecode >>\nstream\n%s\nendstream\nendobj\n", len(xrefData), xrefData)
	fmt.Fprintf(&b, "startxref\n%d\n%%%%EOF\n", xrefAt)
	return b.Bytes()
}

// onePageHOCR is hOCR for a 300x200 pt page at 300 dpi.
func onePageHOCR() *hocr.HOCR {
	return &hocr.HOCR{Pages: []hocr.Page{{PageNumber: 1, BBox: hocr.BoundingBox{X2: 1250, Y2: 833}}}}
}

func applyOCRRecovered(data []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	config := pdfocr.DefaultConfig()
	config.LogWarnings = false
	return pdfocr.ApplyOCR(data, onePageHOCR(), config)
}

func TestNormalizePDFRepairsNameDA(t *testing.T) {
	original := iosAnnotatedPDF()
	err := api.Validate(bytes.NewReader(original), nil)
	require.Error(t, err, "the fixture must reproduce pdfcpu's refusal")
	assert.Contains(t, err.Error(), "invalid type types.Name")

	normalized, err := normalizePDF(original)
	require.NoError(t, err)
	require.NoError(t, api.Validate(bytes.NewReader(normalized), nil))
	assert.Contains(t, string(normalized), "/DA(/Helvetica 18 Tf 0 g)")
	require.NoError(t, checkRebuiltPDF(context.Background(), original, normalized))
}

func TestDownloadDocumentAsPDFSplitsAnIOSAnnotatedPDF(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	const documentID = 2019
	env.setMockResponse(fmt.Sprintf("/api/documents/%d/download/", documentID), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(iosAnnotatedPDF())
	})
	env.client.CacheFolder = t.TempDir()

	paths, data, totalPages, err := env.client.DownloadDocumentAsPDF(context.Background(), documentID, 0, true)
	require.NoError(t, err)
	assert.Len(t, paths, 1)
	assert.Equal(t, 1, totalPages)
	assert.Equal(t, iosAnnotatedPDF(), data, "the original is passed on unchanged")
}

func TestNormalizePDFMakesGofpdiReadablePDFs(t *testing.T) {
	for name, original := range map[string][]byte{
		"ASCII85 page content":    ascii85PDF(),
		"length in object stream": objectStreamPDF(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := applyOCRRecovered(original)
			require.Error(t, err, "the fixture must reproduce gofpdi's failure")

			normalized, err := normalizePDF(original)
			require.NoError(t, err)
			rebuilt, err := applyOCRRecovered(normalized)
			require.NoError(t, err)
			require.NoError(t, checkRebuiltPDF(context.Background(), original, rebuilt))
		})
	}
}

func TestProcessDocumentOCRVersionsPDFsGofpdiCannotRead(t *testing.T) {
	for name, original := range map[string][]byte{
		"ASCII85 page content":    ascii85PDF(),
		"length in object stream": objectStreamPDF(),
	} {
		t.Run(name, func(t *testing.T) {
			doc, uploaded := runVersionOCROn(t, original, "Invoice")
			assert.Equal(t, "versioned", doc.PDFAction, doc.PDFDetail)
			assert.NotEmpty(t, uploaded)
		})
	}
}
