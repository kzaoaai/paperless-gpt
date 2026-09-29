package main

import (
	"context"
	"regexp"
	"strings"
	"unicode"

	"github.com/sirupsen/logrus"

	"paperless-gpt/internal/pdfrender"
)

// keepExistingTextCoverage is the share of the recognized words, overall and
// in each script, that a digital PDF must already hold for its own text to be
// kept. A digital PDF's text is exact, which OCR can at best match. Scans,
// whose text (if any) is an OCR layer over the image, and digital PDFs missing
// text (Arabic drawn without extractable text, for example) still get the
// recognized text.
const keepExistingTextCoverage = 0.9

// scannedPageImageCover is the share of a page an image must cover for the
// page to count as scanned.
const scannedPageImageCover = 0.5

// keepsExistingText reports whether pdfData is a digital PDF (no page is a
// scanned image) whose own text already holds the recognized text, overall
// and in each script, and the overall share it holds. A PDF that cannot be
// read is not kept.
func keepsExistingText(ctx context.Context, logger *logrus.Entry, pdfData []byte, recognized string) (bool, float64) {
	if len(pdfData) == 0 {
		return false, 0
	}
	doc, err := pdfrender.Open(ctx, pdfData)
	if err != nil {
		logger.WithError(err).Warn("Could not read the PDF's own text; treating it as a scan")
		return false, 0
	}
	defer doc.Close()
	var text strings.Builder
	scannedPages := 0
	for i := 0; i < doc.NumPages(); i++ {
		pageText, imageCover, err := doc.PageText(i)
		if err != nil {
			logger.WithError(err).Warn("Could not read the PDF's own text; treating it as a scan")
			return false, 0
		}
		text.WriteString(pageText)
		text.WriteString("\n")
		if imageCover >= scannedPageImageCover {
			scannedPages++
		}
	}
	overall, weakest := wordCoverage(recognized, text.String())
	logger.WithFields(logrus.Fields{
		"scanned_pages":    scannedPages,
		"coverage":         overall,
		"weakest_coverage": weakest,
	}).Debug("Compared the recognized text with the PDF's own text")
	return scannedPages == 0 && overall >= keepExistingTextCoverage && weakest >= keepExistingTextCoverage, overall
}

var wordPattern = regexp.MustCompile(`[\p{L}\p{M}\p{N}]{2,}`)

// minScriptWords is how many recognized words a script needs before its own
// coverage counts.
const minScriptWords = 3

// wordCoverage returns the share of the words of recognized, counted with
// repeats and ignoring case, that also occur in existing: overall, and in the
// script (Latin, Arabic, digits, ...) where the share is lowest among those
// with at least minScriptWords words (1 when there is none).
func wordCoverage(recognized, existing string) (overall, weakest float64) {
	have := map[string]int{}
	for _, w := range wordPattern.FindAllString(strings.ToLower(existing), -1) {
		have[w]++
	}
	type tally struct{ total, found int }
	perScript := map[string]*tally{}
	var all tally
	for _, w := range wordPattern.FindAllString(strings.ToLower(recognized), -1) {
		t := perScript[scriptOf(w)]
		if t == nil {
			t = &tally{}
			perScript[scriptOf(w)] = t
		}
		all.total++
		t.total++
		if have[w] > 0 {
			have[w]--
			all.found++
			t.found++
		}
	}
	if all.total == 0 {
		return 0, 0
	}
	weakest = 1
	for _, t := range perScript {
		if t.total >= minScriptWords {
			weakest = min(weakest, float64(t.found)/float64(t.total))
		}
	}
	return float64(all.found) / float64(all.total), weakest
}

// scriptOf names the script of a word's first character, or "digits".
func scriptOf(word string) string {
	for _, r := range word {
		if unicode.IsDigit(r) {
			return "digits"
		}
		for name, table := range unicode.Scripts {
			if name != "Common" && name != "Inherited" && unicode.Is(table, r) {
				return name
			}
		}
		return "other"
	}
	return "other"
}
