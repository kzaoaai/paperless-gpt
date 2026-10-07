package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"codeberg.org/go-pdf/fpdf"
	"github.com/gardar/ocrchestra/pkg/hocr"
	"github.com/gardar/ocrchestra/pkg/pdfocr"
)

// guardSizes mixes page sizes the way a scan of a card and receipts does.
var guardSizes = [][2]float64{{241, 156}, {598, 843}, {601, 284}}

// guardPDF draws a filled rectangle on each page, leaving out the pages in
// blank, sized as given.
func guardPDF(t *testing.T, sizes [][2]float64, blank ...int) []byte {
	t.Helper()
	pdf := fpdf.New("P", "pt", "", "")
	for i, s := range sizes {
		pdf.AddPageFormat("P", fpdf.SizeType{Wd: s[0], Ht: s[1]})
		isBlank := false
		for _, b := range blank {
			isBlank = isBlank || b == i
		}
		if !isBlank {
			pdf.Rect(10, 10, s[0]-20, s[1]-20, "F")
		}
	}
	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCheckRebuiltPDF(t *testing.T) {
	ctx := context.Background()
	original := guardPDF(t, guardSizes)

	// A searchable PDF rebuilt by ocrchestra, from hOCR measured in pixels.
	var h hocr.HOCR
	for i, s := range guardSizes {
		h.Pages = append(h.Pages, hocr.Page{PageNumber: i + 1, BBox: hocr.BoundingBox{X2: s[0] * 200 / 72, Y2: s[1] * 200 / 72}})
	}
	config := pdfocr.DefaultConfig()
	config.LogWarnings = false
	rebuilt, err := pdfocr.ApplyOCR(original, &h, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkRebuiltPDF(ctx, original, rebuilt); err != nil {
		t.Errorf("faithful rebuild refused: %v", err)
	}

	scaled := [][2]float64{guardSizes[0], {guardSizes[1][0] * 2, guardSizes[1][1] * 2}, guardSizes[2]}
	for name, tc := range map[string]struct {
		rebuilt []byte
		want    string
	}{
		"page dropped": {guardPDF(t, guardSizes[:2]), "has 2 pages"},
		"page resized": {guardPDF(t, scaled), "page 2 is"},
		"page blanked": {guardPDF(t, guardSizes, 2), "page 3 renders differently"},
	} {
		err := checkRebuiltPDF(ctx, original, tc.rebuilt)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestApplyOCRRefusesTurnedPage(t *testing.T) {
	// Document AI turns a sideways scan upright, so its hOCR page is
	// landscape for a portrait PDF page; ocrchestra must refuse it.
	original := guardPDF(t, guardSizes)
	var h hocr.HOCR
	for i, s := range guardSizes {
		if i == 1 {
			s = [2]float64{s[1], s[0]}
		}
		h.Pages = append(h.Pages, hocr.Page{PageNumber: i + 1, BBox: hocr.BoundingBox{X2: s[0] * 200 / 72, Y2: s[1] * 200 / 72}})
	}
	config := pdfocr.DefaultConfig()
	config.LogWarnings = false
	if _, err := pdfocr.ApplyOCR(original, &h, config); !errors.Is(err, pdfocr.ErrPageGeometry) {
		t.Errorf("err = %v, want pdfocr.ErrPageGeometry", err)
	}
}
