package main

// Why a document carries the fail tag. paperless-gpt marks a document failed in
// two ways: paperless-ngx refused one of the suggested values (the rest was
// applied), or processing gave up on the document. Either way the reason used
// to exist only as a log line. It is kept here, with the value that was
// refused, so the Tidy page can say what happened and offer the fix.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	failureRejectedField = "rejected_field"
	failureGaveUp        = "gave_up"
)

// ProcessingFailure is one reason a document was marked failed.
type ProcessingFailure struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	DocumentID int       `gorm:"index" json:"document_id"`
	Kind       string    `gorm:"size:32" json:"kind"`
	// rejected_field: "custom_fields" with the field's id, or the name of a
	// document field (created, title, correspondent); the value that was sent
	// (text as it is, anything else as JSON); Reason is paperless-ngx's message.
	Field   string `gorm:"size:64" json:"field,omitempty"`
	FieldID int    `json:"field_id,omitempty"`
	Value   string `gorm:"type:TEXT" json:"value,omitempty"`
	// gave_up: the tag the document was queued under, and why it left the queue.
	Stage  string `gorm:"size:255" json:"stage,omitempty"`
	Reason string `gorm:"type:TEXT" json:"reason"`
	// Set once someone dealt with it from the Tidy page (the logged action).
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy uint       `json:"resolved_by,omitempty"`
}

// saveProcessingFailures stores the records, creating the table on first use so
// this works whichever part of the program opened the database. A failure to
// record is logged and nothing more: it must never change how a document is
// processed.
func saveProcessingFailures(db *gorm.DB, rows []ProcessingFailure) {
	if db == nil || len(rows) == 0 {
		return
	}
	if !db.Migrator().HasTable(&ProcessingFailure{}) {
		if err := db.AutoMigrate(&ProcessingFailure{}); err != nil {
			log.Warnf("Could not create the processing-failure table: %v", err)
			return
		}
	}
	if err := db.Create(&rows).Error; err != nil {
		log.Warnf("Could not record why document %d was marked failed: %v", rows[0].DocumentID, err)
	}
}

func failureValueText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(buf)
}

// paperlessErrorText flattens a paperless-ngx validation error (a string, a
// list of them, or an object of lists such as {"non_field_errors": [...]})
// into one line.
func paperlessErrorText(v any) string {
	var parts []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if s := strings.TrimSpace(x); s != "" {
				parts = append(parts, s)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return strings.Join(parts, " ")
}

// recordRejectedFields notes the fields paperless-ngx refused in one update,
// with the values that were sent. It is called before those fields are
// stripped from the update, with the same parse of the 400 response.
func recordRejectedFields(db *gorm.DB, documentID int, updatedFields map[string]interface{}, body []byte, scalarFields map[string]bool, customFieldIndices []int) {
	if db == nil {
		return
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	var rows []ProcessingFailure
	for field := range scalarFields {
		value, present := updatedFields[field]
		if !present {
			continue
		}
		rows = append(rows, ProcessingFailure{DocumentID: documentID, Kind: failureRejectedField, Field: field,
			Value: failureValueText(value), Reason: paperlessErrorText(raw[field])})
	}
	if cf, ok := updatedFields["custom_fields"].([]CustomFieldResponse); ok {
		for _, idx := range customFieldIndices {
			if idx < 0 || idx >= len(cf) {
				continue
			}
			var reason any
			switch errs := raw["custom_fields"].(type) {
			case []any:
				if idx < len(errs) {
					reason = errs[idx]
				}
			case map[string]any:
				reason = errs[strconv.Itoa(idx)]
			}
			rows = append(rows, ProcessingFailure{DocumentID: documentID, Kind: failureRejectedField, Field: "custom_fields",
				FieldID: cf[idx].Field, Value: failureValueText(cf[idx].Value), Reason: paperlessErrorText(reason)})
		}
	}
	saveProcessingFailures(db, rows)
}

// recordRejectedValue notes one suggested value that could not be used for a
// reason paperless-gpt found itself (a correspondent it could not create).
func recordRejectedValue(db *gorm.DB, documentID int, field, value string, cause error) {
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	saveProcessingFailures(db, []ProcessingFailure{{DocumentID: documentID, Kind: failureRejectedField, Field: field, Value: value, Reason: reason}})
}

// recordProcessingGaveUp notes that a document left the queue it was in (the
// tag it was picked up by) without being processed.
func recordProcessingGaveUp(db *gorm.DB, documentID int, stage, reason string) {
	saveProcessingFailures(db, []ProcessingFailure{{DocumentID: documentID, Kind: failureGaveUp, Stage: stage, Reason: reason}})
}
