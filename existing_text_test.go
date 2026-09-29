package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWordCoverage(t *testing.T) {
	for _, c := range []struct {
		recognized, existing string
		overall, weakest     float64
	}{
		{"Invoice total 12.50", "INVOICE Total 12.50 due", 1, 1},
		{"Invoice total due", "Invoice", 1.0 / 3, 1.0 / 3},
		{"the the the the", "the", 0.25, 0.25}, // repeats count
		// A few Arabic words missing among many Latin ones: the overall
		// share looks fine, the Arabic share does not.
		{"one two three four five six seven eight nine ten اسم الشركة العنوان", "one two three four five six seven eight nine ten", 10.0 / 13, 0},
		{"one two three four five six seven eight nine ten اسم", "one two three four five six seven eight nine ten", 10.0 / 11, 1}, // too few Arabic words to count on their own
		{"a b c", "a b c", 0, 0}, // single characters are not words
		{"", "anything", 0, 0},
	} {
		overall, weakest := wordCoverage(c.recognized, c.existing)
		assert.InDelta(t, c.overall, overall, 1e-9, "overall: %q in %q", c.recognized, c.existing)
		assert.InDelta(t, c.weakest, weakest, 1e-9, "weakest: %q in %q", c.recognized, c.existing)
	}
}

// scannedPDF builds a one-page PDF like a scan with an OCR layer: an image
// covering the whole page, with the text over it.
func scannedPDF(text string) []byte {
	return onePagePDF(fmt.Sprintf("q 300 0 0 200 0 0 cm /Im1 Do Q\nBT 3 Tr /F1 12 Tf 20 100 Td (%s) Tj ET\n", text))
}

// digitalPDF builds a one-page PDF with the text drawn as text, as an export
// is.
func digitalPDF(text string) []byte {
	return onePagePDF(fmt.Sprintf("BT /F1 8 Tf 10 100 Td (%s) Tj ET\n", text))
}

func onePagePDF(content string) []byte {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> /XObject << /Im1 6 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Type /XObject /Subtype /Image /Width 1 /Height 1 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 1 >>\nstream\n\xffendstream",
	}
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

// tests/pdf/sample.pdf is a digital PDF (exported from Pages) whose visible
// text includes "My little example note".
func TestProcessDocumentOCRKeepsTheTextOfADigitalPDF(t *testing.T) {
	doc, uploaded := runVersionOCR(t, "example")

	assert.Equal(t, "kept", doc.PDFAction)
	assert.Contains(t, doc.PDFDetail, "100% of the recognized words")
	assert.False(t, doc.VersionAdded)
	assert.Nil(t, uploaded, "no version for a digital PDF whose text already holds the recognized text")
}

func TestProcessDocumentOCRVersionsAScanWhoseOCRLayerMatches(t *testing.T) {
	doc, uploaded := runVersionOCROn(t, scannedPDF("Invoice"), "Invoice")

	assert.Equal(t, "versioned", doc.PDFAction, doc.PDFDetail)
	assert.NotEmpty(t, uploaded, "a scan's OCR layer is not trusted, however well it matches")
}

// A digital PDF can draw some text without making it extractable, as with
// the Arabic on some exported reports: the few Arabic words it lacks must not
// hide behind the many Latin words it has.
func TestProcessDocumentOCRVersionsADigitalPDFMissingItsArabic(t *testing.T) {
	latin := strings.Repeat("statement account balance transfer amount ", 6) // 30 words
	doc, uploaded := runVersionOCROn(t, digitalPDF(latin), latin+"اسم الشركة العنوان")

	assert.Equal(t, "versioned", doc.PDFAction, doc.PDFDetail)
	assert.NotEmpty(t, uploaded)
}

func TestAutoOCRKeepsADigitalPDFsText(t *testing.T) {
	const documentID = 3110
	e := newVersionTextEnv(t, documentID, "paperless-ngx's extraction of the PDF")

	runAutoOCRRecognizing(t, e, documentID, "paperless-ngx's extraction of the PDF", "example")

	assert.Zero(t, e.uploads, "no version is uploaded")
	assert.Empty(t, contentWrites(e.patches), "the PDF's own text stays the content")
	require.NotEmpty(t, e.patches, "the queue tag is still handed over")
}
