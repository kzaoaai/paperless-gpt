package main

import (
	"bytes"
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/filter"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// normalizePDF rewrites a PDF that one of the two strict readers on the OCR
// path refuses into one they accept, without changing how it looks. It is a
// retry, used only after the original failed: the original is never changed,
// and a searchable PDF built on the rewrite is still checked against the
// original (checkRebuiltPDF).
//
//   - pdfcpu (page split) requires an annotation's or form field's /DA to be a
//     string, even in relaxed mode. iOS Quartz writes it as a name
//     (/DA /Helvetica#2018#20Tf#200#20g), whose text is the intended string
//     without its leading slash.
//   - gofpdi (ApplyOCR) reads only classic xref tables and objects outside
//     object streams, and decodes only FlateDecode page content. The rewrite
//     has a classic xref table and no object streams, and every stream whose
//     filters are all lossless is decoded and stored as FlateDecode.
func normalizePDF(data []byte) ([]byte, error) {
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	conf.WriteObjectStream = false
	conf.WriteXRefStream = false

	ctx, err := api.ReadContext(bytes.NewReader(data), conf)
	if err != nil {
		return nil, fmt.Errorf("reading PDF: %w", err)
	}
	for _, entry := range ctx.XRefTable.Table {
		if entry == nil || entry.Free || entry.Object == nil {
			continue
		}
		repairDA(entry.Object)
		if sd, ok := entry.Object.(types.StreamDict); ok {
			if err := reencodeFlate(&sd); err != nil {
				return nil, err
			}
			entry.Object = sd
		}
	}
	if err := api.ValidateContext(ctx); err != nil {
		return nil, fmt.Errorf("validating PDF: %w", err)
	}
	var out bytes.Buffer
	if err := api.WriteContext(ctx, &out); err != nil {
		return nil, fmt.Errorf("writing PDF: %w", err)
	}
	return out.Bytes(), nil
}

// repairDA turns every /DA name in o, and in the dicts and arrays inside it,
// into the string it stands for.
func repairDA(o types.Object) {
	switch o := o.(type) {
	case types.Dict:
		for k, v := range o {
			if name, ok := v.(types.Name); ok && k == "DA" {
				if da, err := types.Escape("/" + string(name)); err == nil {
					o[k] = types.StringLiteral(*da)
				}
				continue
			}
			repairDA(v)
		}
	case types.StreamDict:
		repairDA(o.Dict)
	case types.Array:
		for _, v := range o {
			repairDA(v)
		}
	}
}

// losslessFilters are the filters a stream can be decoded from and stored as
// FlateDecode without changing its content.
var losslessFilters = map[string]bool{
	filter.ASCII85:   true,
	filter.ASCIIHex:  true,
	filter.RunLength: true,
	filter.LZW:       true,
	filter.Flate:     true,
}

// reencodeFlate stores sd as FlateDecode when its filters are all lossless
// and one of them is not FlateDecode. Image codecs (DCT, JPX, CCITT, JBIG2)
// stay as they are: page content never uses them, and gofpdi copies such
// streams without decoding them.
func reencodeFlate(sd *types.StreamDict) error {
	if t := sd.Type(); t != nil && (*t == "ObjStm" || *t == "XRef") {
		return nil // not written: the rewrite has neither
	}
	onlyFlate := true
	for _, f := range sd.FilterPipeline {
		if !losslessFilters[f.Name] {
			return nil
		}
		onlyFlate = onlyFlate && f.Name == filter.Flate
	}
	if onlyFlate {
		return nil
	}
	if err := sd.Decode(); err != nil {
		return fmt.Errorf("decoding stream: %w", err)
	}
	sd.FilterPipeline = []types.PDFFilter{{Name: filter.Flate}}
	sd.Dict["Filter"] = types.Name(filter.Flate)
	delete(sd.Dict, "DecodeParms")
	if err := sd.Encode(); err != nil {
		return fmt.Errorf("encoding stream: %w", err)
	}
	return nil
}
