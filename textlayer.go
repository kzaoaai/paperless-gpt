package main

import (
	"github.com/gardar/ocrchestra/pkg/hocr"
	"github.com/gardar/ocrchestra/pkg/pdfocr"
)

// textLayerFont draws the searchable-PDF text layer. ocrchestra's default font,
// the PDF core font Helvetica, encodes only ISO-8859-1: anything else (Arabic,
// ...) would be written as mojibake, which paperless-ngx then extracts as the
// document's text. The embedded Unicode font encodes every character of the
// Basic Multilingual Plane (one it has no glyph for, e.g. CJK or Thai, is drawn
// as an invisible box and still extracts as itself) and draws right-to-left
// text in visual order.
var textLayerFont = pdfocr.UnicodeFont

// unencodableWords counts the words of doc, and the words with a character the
// text layer cannot encode (beyond the Basic Multilingual Plane, e.g. emoji),
// which it would leave out. A document with any such word gets no searchable
// PDF.
func unencodableWords(doc *hocr.HOCR) (unencodable, total int) {
	if doc == nil {
		return 0, 0
	}
	words := func(words []hocr.Word) {
		for _, word := range words {
			if len(pdfocr.UnsupportedRunes(word.Text, textLayerFont)) > 0 {
				unencodable++
			}
			total++
		}
	}
	lines := func(lines []hocr.Line) {
		for _, line := range lines {
			words(line.Words)
		}
	}
	paragraphs := func(paragraphs []hocr.Paragraph) {
		for _, paragraph := range paragraphs {
			lines(paragraph.Lines)
			words(paragraph.Words)
		}
	}
	for _, page := range doc.Pages {
		for _, area := range page.Areas {
			paragraphs(area.Paragraphs)
			lines(area.Lines)
			words(area.Words)
		}
		paragraphs(page.Paragraphs)
		lines(page.Lines)
	}
	return unencodable, total
}
