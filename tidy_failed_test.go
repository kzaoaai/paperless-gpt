package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordRejectedFields(t *testing.T) {
	db, err := InitializeTestDB()
	require.NoError(t, err)
	long := strings.Repeat("x", 132)
	updated := map[string]interface{}{
		"title":   "A title",
		"created": "2026-02-31",
		"custom_fields": []CustomFieldResponse{
			{Field: 22, Value: "kept"},
			{Field: 1, Value: long},
			{Field: 17, Value: 12.5},
		},
	}
	// The shape paperless-ngx 3.x answers with: only the failing entries,
	// keyed by their index.
	body := []byte(`{"created":["Date has wrong format."],"custom_fields":{"1":{"non_field_errors":["Ensure this value has at most 128 characters (it has 132)."]},"2":{"value":["Not a valid amount."]}}}`)
	scalar, cfIdx, unrecoverable := parsePaperlessValidationErrors(body)
	require.False(t, unrecoverable)
	db.Where("document_id IN ?", []int{2260, 7, 8}).Delete(&ProcessingFailure{})
	recordRejectedFields(db, 2260, updated, body, scalar, cfIdx)
	recordProcessingGaveUp(db, 7, "paperless-gpt-ocr-auto", "OCR failed 3 times")

	var rows []ProcessingFailure
	// The test database is shared with other tests, so read back only these documents.
	require.NoError(t, db.Where("document_id IN ?", []int{2260, 7}).Order("document_id, field, field_id").Find(&rows).Error)
	require.Len(t, rows, 4)
	assert.Equal(t, ProcessingFailure{ID: rows[0].ID, CreatedAt: rows[0].CreatedAt, DocumentID: 7, Kind: failureGaveUp,
		Stage: "paperless-gpt-ocr-auto", Reason: "OCR failed 3 times"}, rows[0])
	assert.Equal(t, "created", rows[1].Field)
	assert.Equal(t, "2026-02-31", rows[1].Value)
	assert.Equal(t, "Date has wrong format.", rows[1].Reason)
	assert.Equal(t, 1, rows[2].FieldID)
	assert.Equal(t, long, rows[2].Value)
	assert.Equal(t, "Ensure this value has at most 128 characters (it has 132).", rows[2].Reason)
	assert.Equal(t, 17, rows[3].FieldID)
	assert.Equal(t, "12.5", rows[3].Value)
	assert.Equal(t, "Not a valid amount.", rows[3].Reason)

	// The older list shape, and no database at all.
	recordRejectedFields(db, 8, map[string]interface{}{"custom_fields": []CustomFieldResponse{{Field: 5, Value: "a"}, {Field: 6, Value: "b"}}},
		[]byte(`{"custom_fields":[{},{"value":["Bad."]}]}`), nil, []int{1})
	var one ProcessingFailure
	require.NoError(t, db.Where("document_id = 8").First(&one).Error)
	assert.Equal(t, 6, one.FieldID)
	assert.Equal(t, "Bad.", one.Reason)
	recordRejectedFields(nil, 9, updated, body, scalar, cfIdx)
}

func failedFixture(t *testing.T) (*fakePaperless, *tidyService) {
	f := newFakePaperless(t)
	f.addObject("tags", 1, "paperless-gpt-auto", nil)
	f.addObject("tags", 2, "paperless-gpt-failed", nil)
	f.addObject("tags", 3, "Insurance", nil)
	f.addObject("tags", 4, "paperless-gpt-ocr-auto", nil)
	f.addObject("custom_fields", 1, "Details", map[string]any{"data_type": "string"})
	f.addObject("custom_fields", 22, "Category", map[string]any{"data_type": "select",
		"extra_data": map[string]any{"select_options": []map[string]any{{"id": "abc", "label": "Home"}}}})
	// 50: one value refused. 51: gave up on OCR. 52: marked, no record. 53: not marked.
	f.addDoc(tidyDoc{ID: 50, Title: "Certificate of insurance", Added: "2026-11-17T19:52:28Z", Tags: []int{2, 3},
		CustomFields: []tidyFieldValue{{Field: 22, Value: json.RawMessage(`"abc"`)}}})
	f.addDoc(tidyDoc{ID: 51, Title: "Scan", Added: "2026-11-16T10:00:00Z", Tags: []int{2}})
	f.addDoc(tidyDoc{ID: 52, Title: "Old one", Added: "2025-01-01T10:00:00Z", Tags: []int{2, 3}})
	f.addDoc(tidyDoc{ID: 53, Title: "Fine", Added: "2025-01-01T10:00:00Z", Tags: []int{3}})
	s := newTestTidy(t, f)
	s.configRefs = func() tidyConfigRefs {
		return tidyConfigRefs{TagNames: map[string]string{}, CustomFieldIDs: map[int]string{},
			AutoTag: "paperless-gpt-auto", OCRTag: "paperless-gpt-ocr-auto", FailTag: "paperless-gpt-failed"}
	}
	// The test database is shared between tests: start from no records.
	s.db.Where("document_id IN ?", []int{50, 51, 52, 53}).Delete(&ProcessingFailure{})
	saveProcessingFailures(s.db, []ProcessingFailure{
		{DocumentID: 50, Kind: failureRejectedField, Field: "custom_fields", FieldID: 1, Value: strings.Repeat("y", 132),
			Reason: "Ensure this value has at most 128 characters (it has 132)."},
		{DocumentID: 51, Kind: failureGaveUp, Stage: "paperless-gpt-ocr-auto", Reason: "OCR failed 3 times"},
	})
	return f, s
}

func TestTidyFailedViewExplains(t *testing.T) {
	_, s := failedFixture(t)
	view, err := s.failedView(context.Background())
	require.NoError(t, err)
	require.Len(t, view.Items, 3)
	by := map[int]tidyFailedDoc{}
	for _, it := range view.Items {
		by[it.DocID] = it
	}

	a := by[50]
	assert.Equal(t, failureRejectedField, a.Kind)
	assert.Equal(t, "paperless-ngx refused one value that paperless-gpt suggested (Details). Everything else was applied.", a.Summary)
	require.Len(t, a.Fields, 1)
	assert.Equal(t, "Details", a.Fields[0].Name)
	assert.Equal(t, 128, a.Fields[0].MaxLength)
	assert.True(t, a.Fields[0].Editable)
	assert.Equal(t, "The text is 132 characters and this field holds 128 at most. Shorten it and save.", a.Fields[0].Advice)
	assert.Equal(t, "", a.Fields[0].Current)
	assert.Equal(t, "paperless-gpt-auto", a.RetryTag)
	require.NotNil(t, a.FailedAt)

	b := by[51]
	assert.Equal(t, failureGaveUp, b.Kind)
	assert.Equal(t, "paperless-gpt stopped working on this document: OCR failed 3 times.", b.Summary)
	assert.Contains(t, b.Advice, "OCR provider failed")
	assert.Equal(t, "paperless-gpt-ocr-auto", b.RetryTag)

	c := by[52]
	assert.Equal(t, "unknown", c.Kind)
	assert.Nil(t, c.FailedAt)
	assert.Contains(t, c.Summary, "no record")
}

func TestTidyFailedFixClearRetryUndo(t *testing.T) {
	f, s := failedFixture(t)
	ctx := context.Background()

	// A value that is still too long changes nothing and says why.
	_, err := s.fixFailed(ctx, 50, []tidyFailedValue{{Field: "custom_fields", FieldID: 1, Value: strings.Repeat("z", 130)}})
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Contains(t, refusal.Message, "at most 128 characters")
	assert.Equal(t, []int{2, 3}, f.docs[50].Tags)
	assert.Len(t, f.docs[50].CustomFields, 1)

	// A value that fits is saved next to the field already there, and the mark goes.
	a, err := s.fixFailed(ctx, 50, []tidyFailedValue{{Field: "custom_fields", FieldID: 1, Value: " Master policy, 2026-2027 "}})
	require.NoError(t, err)
	assert.Equal(t, "Set Details on document 50 and cleared its failed mark", a.Summary)
	assert.Equal(t, []int{3}, f.docs[50].Tags)
	require.Len(t, f.docs[50].CustomFields, 2)
	assert.JSONEq(t, `"abc"`, string(f.docs[50].CustomFields[0].Value))
	assert.JSONEq(t, `"Master policy, 2026-2027"`, string(f.docs[50].CustomFields[1].Value))
	view, err := s.failedView(ctx)
	require.NoError(t, err)
	assert.Len(t, view.Items, 2)

	// Undo: the field is gone again, the mark and its explanation are back.
	_, err = s.undo(ctx, a.ID, false)
	require.NoError(t, err)
	assert.Equal(t, []int{2, 3}, f.docs[50].Tags)
	assert.Len(t, f.docs[50].CustomFields, 1)
	view, err = s.failedView(ctx)
	require.NoError(t, err)
	require.Len(t, view.Items, 3)
	assert.Equal(t, failureRejectedField, view.Items[2].Kind)

	// Run again: 51 goes back under the tag it was queued by, 52 under AUTO_TAG;
	// 53 is not marked and is left alone.
	a, err = s.clearFailed(ctx, []int{51, 52, 53}, true)
	require.NoError(t, err)
	assert.Equal(t, "Queued 2 failed documents for paperless-gpt again", a.Summary)
	assert.Equal(t, []int{4}, f.docs[51].Tags)
	assert.Equal(t, []int{1, 3}, f.docs[52].Tags)
	assert.Equal(t, []int{3}, f.docs[53].Tags)

	// paperless-gpt has since taken 52 out of the queue: undo leaves it and says so.
	f.mu.Lock()
	f.docs[52].Tags = []int{3}
	f.mu.Unlock()
	_, err = s.undo(ctx, a.ID, false)
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, []int{52}, refusal.DriftedDocs)
	_, err = s.undo(ctx, a.ID, true)
	require.NoError(t, err)
	assert.Equal(t, []int{2}, f.docs[51].Tags)
	assert.Equal(t, []int{3}, f.docs[52].Tags)

	// Clear: only the mark goes.
	a, err = s.clearFailed(ctx, []int{51}, false)
	require.NoError(t, err)
	assert.Equal(t, "Cleared the failed mark on 1 document", a.Summary)
	assert.Empty(t, f.docs[51].Tags)
	_, err = s.clearFailed(ctx, []int{53}, false)
	require.ErrorAs(t, err, &refusal)

	// Read-only: nothing is written.
	s.writes = false
	_, err = s.clearFailed(ctx, []int{50}, false)
	require.ErrorAs(t, err, &refusal)
	_, err = s.fixFailed(ctx, 50, []tidyFailedValue{{Field: "custom_fields", FieldID: 1, Value: "x"}})
	require.ErrorAs(t, err, &refusal)
}

func TestTidySetCreatedDatesAndUndo(t *testing.T) {
	f := newFakePaperless(t)
	f.addDoc(tidyDoc{ID: 300, Title: "three days after", Created: "2024-03-13", Added: "2024-03-10T09:00:00+02:00"})
	f.addDoc(tidyDoc{ID: 301, Title: "fine", Created: "2024-03-01", Added: "2024-03-10T09:00:00+02:00"})
	s := newTestTidy(t, f)
	ctx := context.Background()

	var refusal *tidyRefusal
	_, err := s.setCreatedDates(ctx, []tidyDateSetItem{{DocID: 300, Created: "10/03/2024"}})
	require.ErrorAs(t, err, &refusal)
	_, err = s.setCreatedDates(ctx, []tidyDateSetItem{{DocID: 301, Created: "2024-03-01"}})
	require.ErrorAs(t, err, &refusal)

	a, err := s.setCreatedDates(ctx, []tidyDateSetItem{{DocID: 300, Created: "2024-03-10"}, {DocID: 301, Created: "2024-03-01"}})
	require.NoError(t, err)
	assert.Equal(t, "Set the created date of 1 document; skipped 1 (unchanged or gone)", a.Summary)
	assert.Equal(t, "2024-03-10", f.docs[300].Created)
	checks, err := s.dateChecks(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, checks.Items)
	assert.True(t, checks.Writes)

	_, err = s.undo(ctx, a.ID, false)
	require.NoError(t, err)
	assert.Equal(t, "2024-03-13", f.docs[300].Created)
}
