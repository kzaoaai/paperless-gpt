package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"

	"paperless-gpt/internal/pdfrender"
)

// Rebuilding a PDF must leave every page looking exactly as it did: the text
// layer is invisible, so a searchable PDF renders like its original. A page
// whose size or ink changed was cropped, scaled or dropped while being
// rebuilt (as gofpdi did to every page sized differently from page 1), and
// uploading it would replace the document's visible content.
const (
	// pdfGuardDPI is the resolution pages are compared at: enough to see a
	// cropped or missing page, cheap for large pages.
	pdfGuardDPI = 18
	// pdfGuardSizeTolerance is how far, in points, a rebuilt page's width or
	// height may differ from the original's.
	pdfGuardSizeTolerance = 1.0
	// A rebuilt page's average ink (0 white to 255 black) may differ from
	// the original's by pdfGuardInkTolerance or pdfGuardInkShare of it,
	// whichever is larger. Faithful rebuilds measured within 0.4; cropped or
	// blanked pages differ by 6 to 57.
	pdfGuardInkTolerance = 1.0
	pdfGuardInkShare     = 0.03
)

// errRebuiltPDFDiffers marks a searchable PDF that checkRebuiltPDF refused.
var errRebuiltPDFDiffers = errors.New("searchable PDF differs from the original")

// checkRebuiltPDF reports how rebuilt differs visibly from original, or nil
// when every page has the original's size and ink.
func checkRebuiltPDF(ctx context.Context, original, rebuilt []byte) error {
	orig, err := pdfrender.Open(ctx, original)
	if err != nil {
		return fmt.Errorf("reading the original: %w", err)
	}
	defer orig.Close()
	out, err := pdfrender.Open(ctx, rebuilt)
	if err != nil {
		return fmt.Errorf("reading the searchable PDF: %w", err)
	}
	defer out.Close()

	if orig.NumPages() != out.NumPages() {
		return fmt.Errorf("the searchable PDF has %d pages, the original %d", out.NumPages(), orig.NumPages())
	}
	for i := 0; i < orig.NumPages(); i++ {
		ow, oh, err := orig.PageSize(i)
		if err != nil {
			return err
		}
		rw, rh, err := out.PageSize(i)
		if err != nil {
			return err
		}
		if math.Abs(ow-rw) > pdfGuardSizeTolerance || math.Abs(oh-rh) > pdfGuardSizeTolerance {
			return fmt.Errorf("page %d is %.0fx%.0f pt in the searchable PDF, %.0fx%.0f pt in the original", i+1, rw, rh, ow, oh)
		}
		oInk, err := pageInk(orig, i)
		if err != nil {
			return err
		}
		rInk, err := pageInk(out, i)
		if err != nil {
			return err
		}
		if math.Abs(oInk-rInk) > math.Max(pdfGuardInkTolerance, pdfGuardInkShare*oInk) {
			return fmt.Errorf("page %d renders differently in the searchable PDF (ink %.1f, original %.1f)", i+1, rInk, oInk)
		}
	}
	return nil
}

// pageInk renders a page (0-based) and returns its average darkness, from 0
// (white) to 255 (black).
func pageInk(doc *pdfrender.Document, index int) (float64, error) {
	img, err := doc.RenderDPI(index, pdfGuardDPI)
	if err != nil {
		return 0, err
	}
	return averageInk(img), nil
}

func averageInk(img *image.RGBA) float64 {
	b := img.Bounds()
	if b.Empty() {
		return 0
	}
	var sum float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := img.Pix[img.PixOffset(b.Min.X, y):img.PixOffset(b.Max.X, y)]
		for i := 0; i+3 < len(row); i += 4 {
			// Rec. 601 luma
			sum += 0.299*float64(row[i]) + 0.587*float64(row[i+1]) + 0.114*float64(row[i+2])
		}
	}
	return 255 - sum/float64(b.Dx()*b.Dy())
}
