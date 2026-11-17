package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A text of 132 characters, the length that marked a document failed.
const overLongDetails = "Master policy for the homeowners association: general liability, property and directors and officers cover, 2026-11-04 to 2027-11-04"

func TestFitText(t *testing.T) {
	require.Len(t, []rune(overLongDetails), 132)
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits", "short text", 128, "short text"},
		{"exactly the limit", strings.Repeat("a", 128), 128, strings.Repeat("a", 128)},
		{"a text that fits keeps its spaces", " padded ", 128, " padded "},
		{"cut at the last whole word", overLongDetails, 128,
			"Master policy for the homeowners association: general liability, property and directors and officers cover, 2026-11-04 to"},
		{"the limit falls on a word's end", "one two three", 7, "one two"},
		{"the limit falls inside a word", "one two three", 6, "one"},
		{"a dangling separator goes", "alpha, beta - gamma", 14, "alpha, beta"},
		{"characters are counted, not bytes", "été à la mer", 5, "été à"},
		{"one word longer than the limit", strings.Repeat("x", 20), 8, strings.Repeat("x", 8)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fitText(tt.in, tt.max)
			assert.Equal(t, tt.want, got)
			assert.LessOrEqual(t, len([]rune(got)), tt.max)
		})
	}
}

func TestFitCustomFieldText(t *testing.T) {
	field := CustomFieldSuggestion{ID: 5, Name: "Details"}
	assert.Equal(t, "short", fitCustomFieldText(9, field, "string", "short"))
	cut, ok := fitCustomFieldText(9, field, "string", overLongDetails).(string)
	require.True(t, ok)
	assert.LessOrEqual(t, len([]rune(cut)), customFieldTextMax)
	assert.True(t, strings.HasPrefix(overLongDetails, cut))
	// Only text fields have the limit: a long URL or a non-string is left alone.
	assert.Equal(t, overLongDetails, fitCustomFieldText(9, field, "url", overLongDetails))
	assert.Equal(t, 42, fitCustomFieldText(9, field, "string", 42))
}

// TestUpdateDocuments_CutsOverLongCustomFieldText verifies that a text custom
// field longer than paperless-ngx holds is sent cut to fit, in one PATCH, with
// the other fields, and that the document is not reported as a partial update
// (no fail tag).
func TestUpdateDocuments_CutsOverLongCustomFieldText(t *testing.T) {
	env := newTestEnv(t)
	defer env.teardown()

	env.setMockResponse("/api/tags/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{{"id": 1, "name": autoTag}},
			"next":    nil,
		})
	})
	env.setMockResponse("/api/custom_fields/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"results": []map[string]interface{}{
				{"id": 1, "name": "Amount", "data_type": "monetary"},
				{"id": 5, "name": "Details", "data_type": "string"},
			},
			"next": nil,
		})
	})

	patchCount := 0
	var receivedPatch map[string]interface{}
	env.setMockResponse("/api/documents/9/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": 9, "title": "Test Doc", "tags": []int{1}, "custom_fields": []map[string]interface{}{}, "content": "content",
			})
			return
		}
		if r.Method == "PATCH" {
			patchCount++
			receivedPatch = nil
			json.NewDecoder(r.Body).Decode(&receivedPatch)
			// Refuse an over-long text the way paperless-ngx does.
			if fields, ok := receivedPatch["custom_fields"].([]interface{}); ok {
				for _, raw := range fields {
					if s, ok := raw.(map[string]interface{})["value"].(string); ok && len([]rune(s)) > 128 {
						w.WriteHeader(http.StatusBadRequest)
						w.Write([]byte(`{"custom_fields":{"1":{"non_field_errors":["Ensure this value has at most 128 characters (it has 132)."]}}}`))
						return
					}
				}
			}
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"id": 9})
		}
	})

	suggestion := DocumentSuggestion{
		ID:               9,
		OriginalDocument: Document{ID: 9, Title: "Test Doc", Tags: []string{autoTag}},
		SuggestedTitle:   "Certificate of insurance",
		SuggestedCustomFields: []CustomFieldSuggestion{
			{ID: 1, Name: "Amount", Value: "USD120.00"},
			{ID: 5, Name: "Details", Value: overLongDetails},
		},
		CustomFieldsWriteMode: "update",
	}

	// The test database is shared between tests: start from no records.
	env.db.Where("document_id = ?", 9).Delete(&ProcessingFailure{})
	err := env.client.UpdateDocuments(context.Background(), []DocumentSuggestion{suggestion}, env.db, false)
	require.NoError(t, err, "a text cut to fit must not fail the update or produce a PartialUpdateError")
	require.Equal(t, 1, patchCount, "the update must go through in one PATCH")
	assert.Equal(t, "Certificate of insurance", receivedPatch["title"])

	rawFields, ok := receivedPatch["custom_fields"].([]interface{})
	require.True(t, ok, "PATCH must contain a custom_fields array")
	got := map[int]interface{}{}
	for _, raw := range rawFields {
		entry := raw.(map[string]interface{})
		got[int(entry["field"].(float64))] = entry["value"]
	}
	assert.Equal(t, map[int]interface{}{
		1: "USD120.00",
		5: "Master policy for the homeowners association: general liability, property and directors and officers cover, 2026-11-04 to",
	}, got)

	var recorded int64
	env.db.Model(&ProcessingFailure{}).Where("document_id = ?", 9).Count(&recorded)
	assert.Zero(t, recorded, "nothing was refused, so nothing is recorded as a failure")
}
