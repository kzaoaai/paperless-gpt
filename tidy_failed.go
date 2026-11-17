package main

// Tidy: documents paperless-gpt marked failed. The fail tag alone says nothing,
// so this reads the reasons paperless-gpt recorded (failure_log.go) and turns
// them into what happened, what to do, and the three ways out: correct the
// refused value, clear the mark, or queue the document again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type tidyFailedField struct {
	// "custom_fields" (with FieldID) or the name of a document field.
	Field    string `json:"field"`
	FieldID  int    `json:"field_id,omitempty"`
	Name     string `json:"name"`
	DataType string `json:"data_type,omitempty"`
	// What paperless-gpt sent and what paperless-ngx answered.
	Sent  string `json:"sent"`
	Error string `json:"error"`
	// MaxLength is set when the answer was a length limit.
	MaxLength int    `json:"max_length,omitempty"`
	Advice    string `json:"advice"`
	// Current is what the document holds in that field today.
	Current string `json:"current"`
	// Editable: the dialog can save a corrected value.
	Editable bool `json:"editable"`
}

type tidyFailedDoc struct {
	DocID int    `json:"doc_id"`
	Title string `json:"title"`
	Added string `json:"added"`
	// rejected_field, gave_up, or unknown when paperless-gpt has no record.
	Kind     string            `json:"kind"`
	FailedAt *time.Time        `json:"failed_at,omitempty"`
	Summary  string            `json:"summary"`
	Advice   string            `json:"advice"`
	Fields   []tidyFailedField `json:"fields"`
	// The tag "Run again" puts back on the document; empty when it no longer exists.
	RetryTag string `json:"retry_tag"`
}

type tidyFailedView struct {
	Items   []tidyFailedDoc `json:"items"`
	FailTag string          `json:"fail_tag"`
	Writes  bool            `json:"writes"`
}

// tidyFailedValue is a corrected value from the dialog, as text.
type tidyFailedValue struct {
	Field   string `json:"field"`
	FieldID int    `json:"field_id"`
	Value   string `json:"value"`
}

// tidyFailedUndo is what one document looked like before a failed-document
// action, so undo can put it back.
type tidyFailedUndo struct {
	ID            int              `json:"id"`
	TagsBefore    []int            `json:"tags_before"`
	TagsAfter     []int            `json:"tags_after"`
	FieldsSet     bool             `json:"fields_set,omitempty"`
	FieldsBefore  []tidyFieldValue `json:"fields_before,omitempty"`
	TitleBefore   *string          `json:"title_before,omitempty"`
	CreatedBefore *string          `json:"created_before,omitempty"`
}

var (
	tidyMaxLengthRe = regexp.MustCompile(`at most (\d+) characters`)
	tidyDateRe      = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// Custom-field types whose value the dialog can take as plain text.
	tidyFailedEditable = map[string]bool{"string": true, "url": true, "date": true, "integer": true, "float": true, "monetary": true}
	tidyDocFieldLabels = map[string]string{"title": "Title", "created": "Created date", "correspondent": "Correspondent",
		"document_type": "Document type", "storage_path": "Storage path"}
)

// tidyFieldOptions maps a select field's option ids to their labels.
func tidyFieldOptions(f tidyObject) map[string]string {
	var raw struct {
		ExtraData *struct {
			SelectOptions []SelectOption `json:"select_options"`
		} `json:"extra_data"`
	}
	_ = json.Unmarshal(f.Raw, &raw)
	options := map[string]string{}
	if raw.ExtraData != nil {
		for _, o := range raw.ExtraData.SelectOptions {
			options[o.ID] = o.Label
		}
	}
	return options
}

func tidyFailedFieldEntry(d tidyDoc, r ProcessingFailure, fields map[int]tidyObject) tidyFailedField {
	f := tidyFailedField{Field: r.Field, FieldID: r.FieldID, Sent: r.Value, Error: r.Reason}
	if r.Field == "custom_fields" {
		meta, known := fields[r.FieldID]
		f.Name = fmt.Sprintf("custom field %d", r.FieldID)
		if known {
			f.Name, f.DataType = meta.Name, meta.DataType
			f.Editable = tidyFailedEditable[meta.DataType]
			for _, cf := range d.CustomFields {
				if cf.Field == r.FieldID {
					f.Current, _ = tidyFieldDisplay(cf.Value, tidyFieldOptions(meta))
				}
			}
		}
	} else {
		f.Name = tidyDocFieldLabels[r.Field]
		if f.Name == "" {
			f.Name = r.Field
		}
		switch r.Field {
		case "title":
			f.Current, f.Editable = d.Title, true
		case "created":
			f.Current, f.Editable = d.Created, true
		}
	}
	if m := tidyMaxLengthRe.FindStringSubmatch(r.Reason); m != nil {
		f.MaxLength, _ = strconv.Atoi(m[1])
	}
	switch {
	case f.MaxLength > 0 && f.Editable:
		f.Advice = fmt.Sprintf("The text is %d characters and this field holds %d at most. Shorten it and save.", len([]rune(r.Value)), f.MaxLength)
	case r.Field == "correspondent":
		f.Advice = "paperless-gpt could not create or use this correspondent, usually because one of that name exists that its API user cannot see. Set the correspondent in paperless-ngx, then clear the mark."
	case f.Editable:
		f.Advice = "Correct the value and save, or leave the field as it is and clear the mark."
	default:
		f.Advice = "Set it in paperless-ngx, then clear the mark."
	}
	return f
}

// tidyGaveUpAdvice says what a reason paperless-gpt gave up for means.
func tidyGaveUpAdvice(reason string) string {
	switch {
	case strings.Contains(reason, "no OCR provider"):
		return "A workflow asks for OCR, but paperless-gpt has no OCR provider configured. Configure one, or take OCR out of the workflow, before running it again."
	case strings.HasPrefix(reason, "OCR failed"):
		return "The OCR provider failed on this file every time. Running it again repeats the OCR; if it fails again the file is the likely cause (encrypted, damaged or too large), and paperless-gpt's log has the provider's message."
	case strings.HasPrefix(reason, "suggestion generation failed"):
		return "The model call failed every time: the document may be too long for the model, or the provider was down. Running it again asks the model again."
	case strings.HasPrefix(reason, "update failed"):
		return "paperless-ngx refused the whole update. Running it again sends the same kind of request, so deal with what the message names first."
	}
	return "Running it again puts the document back in paperless-gpt's queue."
}

// tidyFailedEntry explains one fail-tagged document from its open records.
func tidyFailedEntry(d tidyDoc, rows []ProcessingFailure, fields map[int]tidyObject) tidyFailedDoc {
	out := tidyFailedDoc{DocID: d.ID, Title: d.Title, Added: d.Added, Kind: "unknown", Fields: []tidyFailedField{}}
	// A document can be marked more than once before anyone looks. Only the
	// latest round counts (records within an hour of the newest), and of two
	// records for one field the later.
	if len(rows) > 0 {
		newest := rows[len(rows)-1].CreatedAt
		latest := map[string]int{}
		var kept []ProcessingFailure
		for _, r := range rows {
			if newest.Sub(r.CreatedAt) > time.Hour {
				continue
			}
			key := r.Kind + "|" + r.Field + "|" + strconv.Itoa(r.FieldID)
			if i, seen := latest[key]; seen {
				kept[i] = r
				continue
			}
			latest[key] = len(kept)
			kept = append(kept, r)
		}
		rows = kept
	}
	var gaveUp *ProcessingFailure
	for i := range rows {
		r := rows[i]
		at := r.CreatedAt
		if out.FailedAt == nil || at.After(*out.FailedAt) {
			out.FailedAt = &at
		}
		if r.Kind == failureGaveUp {
			gaveUp = &rows[i]
			continue
		}
		out.Fields = append(out.Fields, tidyFailedFieldEntry(d, r, fields))
	}
	switch {
	case gaveUp != nil:
		out.Kind = failureGaveUp
		out.Summary = "paperless-gpt stopped working on this document: " + gaveUp.Reason + "."
		out.Advice = tidyGaveUpAdvice(gaveUp.Reason)
	case len(out.Fields) > 0:
		out.Kind = failureRejectedField
		names := make([]string, len(out.Fields))
		for i, f := range out.Fields {
			names[i] = f.Name
		}
		what := "one value"
		if len(names) > 1 {
			what = fmt.Sprintf("%d values", len(names))
		}
		out.Summary = fmt.Sprintf("paperless-ngx refused %s that paperless-gpt suggested (%s). Everything else was applied.", what, strings.Join(names, ", "))
	default:
		out.Summary = "paperless-gpt has no record of why this document is marked."
		out.Advice = "It was marked before paperless-gpt kept such records, or the tag was set by hand. Open the document, see what is missing, then clear the mark or run it again."
	}
	return out
}

// failedContext reads what every failed-document call needs: the tags, the id
// of the fail tag (0 when it does not exist) and the configured tag names.
func (s *tidyService) failedContext(ctx context.Context) (tags []tidyObject, failID int, cfg tidyConfigRefs, err error) {
	cfg = s.configRefs()
	if tags, err = tidyFetchObjects(ctx, s.client, "tags"); err != nil {
		return nil, 0, cfg, err
	}
	if ids := tidyTagIDsByName(tags, cfg.FailTag); len(ids) > 0 {
		failID = ids[0]
	}
	return tags, failID, cfg, nil
}

// openFailures returns the unresolved records of the given documents.
func (s *tidyService) openFailures(ids []int) (map[int][]ProcessingFailure, error) {
	out := map[int][]ProcessingFailure{}
	if s.db == nil || len(ids) == 0 {
		return out, nil
	}
	var rows []ProcessingFailure
	if err := s.db.Where("document_id IN ? AND resolved_at IS NULL", ids).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.DocumentID] = append(out[r.DocumentID], r)
	}
	return out, nil
}

// tidyRetryTag is the tag that queues a document again: the one it was queued
// under when paperless-gpt gave up, if that tag still exists, else AUTO_TAG.
func tidyRetryTag(rows []ProcessingFailure, tags []tidyObject, cfg tidyConfigRefs) (name string, id int) {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Kind == failureGaveUp && rows[i].Stage != "" {
			if ids := tidyTagIDsByName(tags, rows[i].Stage); len(ids) > 0 {
				return rows[i].Stage, ids[0]
			}
			break
		}
	}
	if ids := tidyTagIDsByName(tags, cfg.AutoTag); len(ids) > 0 {
		return cfg.AutoTag, ids[0]
	}
	return "", 0
}

func (s *tidyService) failedView(ctx context.Context) (*tidyFailedView, error) {
	tags, failID, cfg, err := s.failedContext(ctx)
	if err != nil {
		return nil, err
	}
	out := &tidyFailedView{Items: []tidyFailedDoc{}, FailTag: cfg.FailTag, Writes: s.writes}
	if failID == 0 {
		return out, nil
	}
	docs, err := tidyFetchDocs(ctx, s.client, fmt.Sprintf("tags__id__all=%d", failID))
	if err != nil {
		return nil, err
	}
	fieldObjs, err := tidyFetchObjects(ctx, s.client, "custom_fields")
	if err != nil {
		return nil, err
	}
	fields := make(map[int]tidyObject, len(fieldObjs))
	for _, f := range fieldObjs {
		fields[f.ID] = f
	}
	ids := make([]int, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	records, err := s.openFailures(ids)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		item := tidyFailedEntry(d, records[d.ID], fields)
		item.RetryTag, _ = tidyRetryTag(records[d.ID], tags, cfg)
		out.Items = append(out.Items, item)
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].DocID > out.Items[j].DocID })
	return out, nil
}

// tidyCount is "1 document" or "3 documents".
func tidyCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func tidyWithout(ids []int, drop int) []int {
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			out = append(out, id)
		}
	}
	return out
}

// resolveFailures closes the open records of the documents an action dealt with.
func (s *tidyService) resolveFailures(ids []int, actionID uint) {
	if s.db == nil || len(ids) == 0 || !s.db.Migrator().HasTable(&ProcessingFailure{}) {
		return
	}
	now := time.Now()
	if err := s.db.Model(&ProcessingFailure{}).Where("document_id IN ? AND resolved_at IS NULL", ids).
		Updates(map[string]any{"resolved_at": now, "resolved_by": actionID}).Error; err != nil {
		log.Errorf("tidy: closing failure records: %v", err)
	}
}

// tidyRefusedByPaperless turns a 400 from paperless-ngx into its own words.
func tidyRefusedByPaperless(err error) error {
	var apiErr *tidyAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		return err
	}
	var parsed any
	text := apiErr.Body
	if json.Unmarshal([]byte(apiErr.Body), &parsed) == nil {
		if t := paperlessErrorText(parsed); t != "" {
			text = t
		}
	}
	return tidyRefuse("paperless-ngx refused it: %s", text)
}

// fixFailed saves corrected values on one failed document and clears its mark,
// in one request, so a value paperless-ngx refuses again changes nothing.
func (s *tidyService) fixFailed(ctx context.Context, docID int, values []tidyFailedValue) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, tidyRefuse("nothing to save")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	_, failID, _, err := s.failedContext(ctx)
	if err != nil {
		return nil, err
	}
	docs, err := tidyDocsByIDs(ctx, s.client, []int{docID})
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 || failID == 0 || !tidyHasAny(docs[0].Tags, []int{failID}) {
		return nil, tidyRefuse("document %d is not marked failed", docID)
	}
	d := docs[0]
	fieldObjs, err := tidyFetchObjects(ctx, s.client, "custom_fields")
	if err != nil {
		return nil, err
	}
	types := map[int]tidyObject{}
	for _, f := range fieldObjs {
		types[f.ID] = f
	}

	undo := tidyFailedUndo{ID: d.ID, TagsBefore: d.Tags, TagsAfter: tidyWithout(d.Tags, failID)}
	patch := map[string]any{"tags": undo.TagsAfter}
	custom := append([]tidyFieldValue{}, d.CustomFields...)
	var names []string
	for _, v := range values {
		text := strings.TrimSpace(v.Value)
		if text == "" {
			return nil, tidyRefuse("an empty value cannot be saved; clear the mark instead to leave the field as it is")
		}
		switch v.Field {
		case "custom_fields":
			meta, ok := types[v.FieldID]
			if !ok || !tidyFailedEditable[meta.DataType] {
				return nil, tidyRefuse("custom field %d cannot be set from here", v.FieldID)
			}
			var value any = text
			switch meta.DataType {
			case "integer":
				n, err := strconv.Atoi(text)
				if err != nil {
					return nil, tidyRefuse("%s takes a whole number", meta.Name)
				}
				value = n
			case "float":
				n, err := strconv.ParseFloat(text, 64)
				if err != nil {
					return nil, tidyRefuse("%s takes a number", meta.Name)
				}
				value = n
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			replaced := false
			for i := range custom {
				if custom[i].Field == v.FieldID {
					custom[i].Value, replaced = raw, true
				}
			}
			if !replaced {
				custom = append(custom, tidyFieldValue{Field: v.FieldID, Value: raw})
			}
			undo.FieldsSet, undo.FieldsBefore = true, d.CustomFields
			patch["custom_fields"] = custom
			names = append(names, meta.Name)
		case "title":
			before := d.Title
			undo.TitleBefore = &before
			patch["title"] = text
			names = append(names, "Title")
		case "created":
			if !tidyDateRe.MatchString(text) {
				return nil, tidyRefuse("the created date must look like 2026-01-31")
			}
			before := d.Created
			undo.CreatedBefore = &before
			patch["created"] = text
			names = append(names, "Created date")
		default:
			return nil, tidyRefuse("%s cannot be set from here", v.Field)
		}
	}
	if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/documents/%d/", d.ID), patch, nil); err != nil {
		return nil, tidyRefusedByPaperless(err)
	}
	a := &TidyAction{Action: "failed_fix", Kind: "documents", ObjectID: d.ID, ObjectName: d.Title, Status: "done",
		Summary: fmt.Sprintf("Set %s on document %d and cleared its failed mark", strings.Join(names, ", "), d.ID)}
	s.saveAction(a, &tidyActionDetail{Failed: []tidyFailedUndo{undo}})
	s.resolveFailures([]int{d.ID}, a.ID)
	return a, nil
}

// clearFailed takes the fail tag off documents. With retry it also puts each
// one back in paperless-gpt's queue, which costs a model or OCR call apiece.
func (s *tidyService) clearFailed(ctx context.Context, ids []int, retry bool) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	tags, failID, cfg, err := s.failedContext(ctx)
	if err != nil {
		return nil, err
	}
	docs, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(ids))
	if err != nil {
		return nil, err
	}
	records, err := s.openFailures(ids)
	if err != nil {
		return nil, err
	}
	detail := &tidyActionDetail{}
	for _, d := range docs {
		if failID == 0 || !tidyHasAny(d.Tags, []int{failID}) {
			continue
		}
		after := tidyWithout(d.Tags, failID)
		if retry {
			_, queueID := tidyRetryTag(records[d.ID], tags, cfg)
			if queueID == 0 {
				return nil, tidyRefuse("the tag that queues a document for paperless-gpt (%q) does not exist in paperless-ngx", cfg.AutoTag)
			}
			after = tidyUniqueSorted(append(after, queueID))
		}
		detail.Failed = append(detail.Failed, tidyFailedUndo{ID: d.ID, TagsBefore: d.Tags, TagsAfter: after})
	}
	if len(detail.Failed) == 0 {
		return nil, tidyRefuse("none of the selected documents is marked failed")
	}
	a := &TidyAction{Action: "failed_clear", Kind: "documents", Status: "started",
		Summary: "Cleared the failed mark on " + tidyCount(len(detail.Failed), "document")}
	if retry {
		a.Action, a.Summary = "failed_retry", "Queued "+tidyCount(len(detail.Failed), "failed document")+" for paperless-gpt again"
	}
	if len(detail.Failed) == 1 {
		a.ObjectID = detail.Failed[0].ID
	}
	s.saveAction(a, detail)
	done := make([]int, 0, len(detail.Failed))
	for i, u := range detail.Failed {
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/documents/%d/", u.ID), map[string]any{"tags": u.TagsAfter}, nil); err != nil {
			detail.Failed = detail.Failed[:i] // only the ones that were changed, so undo touches nothing else
			a.Status, a.Error = "partial", fmt.Sprintf("changing the tags of document %d: %v", u.ID, err)
			s.saveAction(a, detail)
			s.resolveFailures(done, a.ID)
			return a, err
		}
		done = append(done, u.ID)
	}
	a.Status = "done"
	s.saveAction(a, detail)
	s.resolveFailures(done, a.ID)
	return a, nil
}

// undoFailed puts tags, and any value a fix replaced, back on the documents
// that still look the way the action left them, and reopens their records.
func (s *tidyService) undoFailed(ctx context.Context, a *TidyAction, d *tidyActionDetail, force bool) (string, error) {
	ids := make([]int, len(d.Failed))
	for i, u := range d.Failed {
		ids[i] = u.ID
	}
	current, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(ids))
	if err != nil {
		return "", err
	}
	now := map[int]tidyDoc{}
	for _, doc := range current {
		now[doc.ID] = doc
	}
	var back []tidyFailedUndo
	var drifted []int
	for _, u := range d.Failed {
		if doc, ok := now[u.ID]; ok && tidySameSet(doc.Tags, u.TagsAfter) {
			back = append(back, u)
		} else {
			drifted = append(drifted, u.ID)
		}
	}
	drifted = tidyUniqueSorted(drifted)
	if len(drifted) > 0 && !force {
		return "", &tidyRefusal{Message: fmt.Sprintf("%d documents changed since; undo the rest anyway?", len(drifted)), DriftedDocs: drifted}
	}
	restored := make([]int, 0, len(back))
	for _, u := range back {
		patch := map[string]any{"tags": u.TagsBefore}
		if u.FieldsSet {
			fields := u.FieldsBefore
			if fields == nil {
				fields = []tidyFieldValue{}
			}
			patch["custom_fields"] = fields
		}
		if u.TitleBefore != nil {
			patch["title"] = *u.TitleBefore
		}
		if u.CreatedBefore != nil {
			patch["created"] = *u.CreatedBefore
		}
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/documents/%d/", u.ID), patch, nil); err != nil {
			return "", fmt.Errorf("restoring document %d: %w", u.ID, err)
		}
		restored = append(restored, u.ID)
	}
	if s.db != nil && len(restored) > 0 && s.db.Migrator().HasTable(&ProcessingFailure{}) {
		s.db.Model(&ProcessingFailure{}).Where("resolved_by = ? AND document_id IN ?", a.ID, restored).
			Updates(map[string]any{"resolved_at": nil, "resolved_by": 0})
	}
	note := "Put " + tidyCount(len(back), "document") + " back as they were, marked failed"
	if len(drifted) > 0 {
		note += fmt.Sprintf("; left %d that changed since: %s", len(drifted), tidyJoinInts(drifted))
	}
	return note, nil
}
