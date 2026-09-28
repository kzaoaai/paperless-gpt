package main

import (
	"strings"

	"github.com/gardar/ocrchestra/pkg/hocr"
	"golang.org/x/text/encoding/charmap"
)

// The searchable-PDF text layer (ocrchestra's pdfocr) draws every word with the
// PDF core font Helvetica in ISO-8859-1. A word outside that character set is
// written as raw UTF-8 bytes and comes out as mojibake, which paperless-ngx then
// extracts and indexes as the document's text. Typographic punctuation, which
// Document AI emits constantly, is folded to ASCII; any other character the layer
// cannot encode (Arabic, CJK, Cyrillic, ...) means no searchable PDF.
var textLayerPunctuation = strings.NewReplacer(
	"‘", "'", "’", "'", "‚", "'", "′", "'",
	"“", "\"", "”", "\"", "„", "\"", "″", "\"",
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "−", "-",
	"…", "...", "•", "*", " ", " ", " ", " ",
)

// prepareTextLayer returns a copy of doc with punctuation folded for the text
// layer, and how many of its words the layer still cannot encode. doc itself is
// not modified.
func prepareTextLayer(doc *hocr.HOCR) (prepared *hocr.HOCR, unencodable, total int) {
	if doc == nil {
		return nil, 0, 0
	}
	f := &textLayerFolder{encoder: charmap.ISO8859_1.NewEncoder()}
	out := *doc
	out.Pages = make([]hocr.Page, len(doc.Pages))
	for i, page := range doc.Pages {
		page.Areas = f.areas(page.Areas)
		page.Paragraphs = f.paragraphs(page.Paragraphs)
		page.Lines = f.lines(page.Lines)
		out.Pages[i] = page
	}
	return &out, f.unencodable, f.total
}

type textLayerFolder struct {
	encoder            interface{ String(string) (string, error) }
	unencodable, total int
}

func (f *textLayerFolder) words(words []hocr.Word) []hocr.Word {
	if words == nil {
		return nil
	}
	out := make([]hocr.Word, len(words))
	for i, word := range words {
		word.Text = textLayerPunctuation.Replace(word.Text)
		if _, err := f.encoder.String(word.Text); err != nil {
			f.unencodable++
		}
		f.total++
		out[i] = word
	}
	return out
}

func (f *textLayerFolder) lines(lines []hocr.Line) []hocr.Line {
	if lines == nil {
		return nil
	}
	out := make([]hocr.Line, len(lines))
	for i, line := range lines {
		line.Words = f.words(line.Words)
		out[i] = line
	}
	return out
}

func (f *textLayerFolder) paragraphs(paragraphs []hocr.Paragraph) []hocr.Paragraph {
	if paragraphs == nil {
		return nil
	}
	out := make([]hocr.Paragraph, len(paragraphs))
	for i, paragraph := range paragraphs {
		paragraph.Lines = f.lines(paragraph.Lines)
		paragraph.Words = f.words(paragraph.Words)
		out[i] = paragraph
	}
	return out
}

func (f *textLayerFolder) areas(areas []hocr.Area) []hocr.Area {
	if areas == nil {
		return nil
	}
	out := make([]hocr.Area, len(areas))
	for i, area := range areas {
		area.Paragraphs = f.paragraphs(area.Paragraphs)
		area.Lines = f.lines(area.Lines)
		area.Words = f.words(area.Words)
		out[i] = area
	}
	return out
}
