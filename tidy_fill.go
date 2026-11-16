package main

// Tidy: fill missing tags and document types. A background run asks
// paperless-gpt's own tag and document-type prompts (nothing else: no title,
// date, correspondent or custom fields) about every document that has no tags
// or no type, and stores the answers. The page lists them; only ticked ones
// are applied, as one logged action that undo reverses. A document on the
// ignore list is never asked about until it is taken off it.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// TidyFillRun is one pass of the tag and type prompts.
type TidyFillRun struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Status    string    `gorm:"size:16" json:"status"` // running, done, stopped
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Calls     int       `json:"calls"` // LLM prompts the run needs: one per missing field
	Results   string    `gorm:"type:TEXT" json:"-"`
}

// TidyFillIgnore is a document a person told Fill to leave alone. The list is
// paperless-gpt's own: nothing about the document changes in paperless-ngx.
type TidyFillIgnore struct {
	DocID     int       `gorm:"primaryKey;autoIncrement:false" json:"doc_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

type tidyFillIgnoreItem struct {
	DocID int    `json:"doc_id"`
	Title string `json:"title"`
}

// tidyFillStatus is what a run would do now, and the ignore list.
type tidyFillStatus struct {
	Documents   int               `json:"documents"`
	WithoutTags int               `json:"without_tags"`
	WithoutType int               `json:"without_type"`
	Calls       int               `json:"calls"`
	Ignored     []tidyFillIgnored `json:"ignored"`
}

// tidyFillIgnored is an ignored document with what it lacks today, so the page
// can tell a document left without tags from one left without a type, or one
// filled by hand since it was ignored.
type tidyFillIgnored struct {
	TidyFillIgnore
	NoTags bool `json:"no_tags"`
	NoType bool `json:"no_type"`
	// Gone: paperless-ngx no longer lists the document (deleted or trashed).
	Gone bool `json:"gone"`
}

type tidyFillResult struct {
	DocID   int      `json:"doc_id"`
	Title   string   `json:"title"`
	AskTags bool     `json:"ask_tags"`
	AskType bool     `json:"ask_type"`
	Tags    []string `json:"tags,omitempty"`
	Type    string   `json:"type,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// tidyFillSuggestFunc runs paperless-gpt's tag and/or type prompt on one
// document and returns its title and the suggested names.
type tidyFillSuggestFunc func(ctx context.Context, docID int, wantTags, wantType bool) (title string, tags []string, docType string, err error)

type tidyFillTag struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type tidyFillItem struct {
	DocID       int           `json:"doc_id"`
	Title       string        `json:"title"`
	AskTags     bool          `json:"ask_tags"`
	Tags        []tidyFillTag `json:"tags"` // the suggested tags that exist, each droppable on the page
	TagNames    []string      `json:"tag_names"`
	TagIDs      []int         `json:"tag_ids"`
	UnknownTags []string      `json:"unknown_tags"` // suggested names that match no tag
	HasTagsNow  bool          `json:"has_tags_now"`
	AskType     bool          `json:"ask_type"`
	TypeName    string        `json:"type_name,omitempty"`
	TypeID      int           `json:"type_id,omitempty"`
	HasTypeNow  bool          `json:"has_type_now"`
	Ignored     bool          `json:"ignored"`
	Error       string        `json:"error,omitempty"`
}

type tidyFillView struct {
	Run   TidyFillRun    `json:"run"`
	Items []tidyFillItem `json:"items"`
}

type tidyFillApplyItem struct {
	DocID  int   `json:"doc_id"`
	TagIDs []int `json:"tag_ids"`
	TypeID int   `json:"type_id"`
}

// tidyFillTargets picks the documents to ask about: no tags or no type, not
// waiting in one of paperless-gpt's queues (it fills those itself) and not on
// the ignore list. Newest first, at most limit when limit > 0.
func tidyFillTargets(docs []tidyDoc, queueTagIDs []int, ignored map[int]bool, limit int) []tidyFillResult {
	sorted := append([]tidyDoc(nil), docs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Added > sorted[j].Added })
	var out []tidyFillResult
	for _, d := range sorted {
		if ignored[d.ID] || tidyHasAny(d.Tags, queueTagIDs) {
			continue
		}
		askTags, askType := len(d.Tags) == 0, d.DocumentType == nil
		if !askTags && !askType {
			continue
		}
		out = append(out, tidyFillResult{DocID: d.ID, AskTags: askTags, AskType: askType})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// fillIgnoredIDs is the ignore list as a set.
func (s *tidyService) fillIgnoredIDs() (map[int]bool, error) {
	out := map[int]bool{}
	if s.db == nil {
		return out, nil
	}
	var ids []int
	if err := s.db.Model(&TidyFillIgnore{}).Pluck("doc_id", &ids).Error; err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// fillTargets lists the documents a run would ask about now.
func (s *tidyService) fillTargets(ctx context.Context, limit int) ([]tidyFillResult, error) {
	targets, _, err := s.fillTargetsAndDocs(ctx, limit)
	return targets, err
}

// fillTargetsAndDocs is fillTargets plus the documents it read, for callers
// that need both without a second pass over the archive.
func (s *tidyService) fillTargetsAndDocs(ctx context.Context, limit int) ([]tidyFillResult, []tidyDoc, error) {
	docs, err := tidyFetchDocs(ctx, s.client, "")
	if err != nil {
		return nil, nil, err
	}
	tags, err := tidyFetchObjects(ctx, s.client, "tags")
	if err != nil {
		return nil, nil, err
	}
	ignored, err := s.fillIgnoredIDs()
	if err != nil {
		return nil, nil, err
	}
	cfg := s.configRefs()
	queue := append(tidyTagIDsByName(tags, cfg.AutoTag), tidyTagIDsByName(tags, cfg.OCRTag)...)
	return tidyFillTargets(docs, queue, ignored, limit), docs, nil
}

// fillStatus counts what a run would ask about and lists the ignored documents.
func (s *tidyService) fillStatus(ctx context.Context) (*tidyFillStatus, error) {
	targets, docs, err := s.fillTargetsAndDocs(ctx, 0)
	if err != nil {
		return nil, err
	}
	st := &tidyFillStatus{Documents: len(targets), Ignored: []tidyFillIgnored{}}
	for _, t := range targets {
		if t.AskTags {
			st.WithoutTags++
			st.Calls++
		}
		if t.AskType {
			st.WithoutType++
			st.Calls++
		}
	}
	if s.db != nil {
		var rows []TidyFillIgnore
		if err := s.db.Order("created_at desc, doc_id desc").Find(&rows).Error; err != nil {
			return nil, err
		}
		byID := make(map[int]tidyDoc, len(docs))
		for _, d := range docs {
			byID[d.ID] = d
		}
		for _, r := range rows {
			it := tidyFillIgnored{TidyFillIgnore: r}
			if d, ok := byID[r.DocID]; ok {
				it.NoTags, it.NoType = len(d.Tags) == 0, d.DocumentType == nil
			} else {
				it.Gone = true
			}
			st.Ignored = append(st.Ignored, it)
		}
	}
	return st, nil
}

// ignoreFill puts documents on the ignore list; one already there stays as it is.
func (s *tidyService) ignoreFill(items []tidyFillIgnoreItem) (int, error) {
	if s.db == nil {
		return 0, tidyRefuse("no database")
	}
	added := 0
	for _, it := range items {
		if it.DocID <= 0 {
			continue
		}
		row := TidyFillIgnore{DocID: it.DocID, Title: strings.TrimSpace(it.Title)}
		res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			return added, res.Error
		}
		added += int(res.RowsAffected)
	}
	return added, nil
}

// unignoreFill takes documents off the ignore list.
func (s *tidyService) unignoreFill(ids []int) (int, error) {
	if s.db == nil {
		return 0, tidyRefuse("no database")
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := s.db.Where("doc_id IN ?", ids).Delete(&TidyFillIgnore{})
	return int(res.RowsAffected), res.Error
}

func (s *tidyService) startFill(ctx context.Context, limit int) (*TidyFillRun, error) {
	if s.fillSuggest == nil {
		return nil, tidyRefuse("no LLM is configured")
	}
	if s.db == nil {
		return nil, tidyRefuse("no database")
	}
	s.splitMu.Lock()
	defer s.splitMu.Unlock()
	if s.splitCancel != nil {
		return nil, tidyRefuse("another LLM run is going; stop it or wait for it")
	}
	targets, err := s.fillTargets(ctx, limit)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, tidyRefuse("nothing to ask about: every document has tags and a type, waits in paperless-gpt's queue, or is ignored")
	}
	calls := 0
	for _, t := range targets {
		if t.AskTags {
			calls++
		}
		if t.AskType {
			calls++
		}
	}
	run := &TidyFillRun{Status: "running", Total: len(targets), Calls: calls}
	if err := s.db.Create(run).Error; err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.splitCancel = cancel
	started := *run
	go s.runFill(runCtx, run, targets)
	return &started, nil
}

func (s *tidyService) runFill(ctx context.Context, run *TidyFillRun, targets []tidyFillResult) {
	defer func() {
		s.splitMu.Lock()
		s.splitCancel = nil
		s.splitMu.Unlock()
	}()
	results := make([]tidyFillResult, 0, len(targets))
	save := func() {
		buf, _ := json.Marshal(results)
		run.Results, run.Done = string(buf), len(results)
		if err := s.db.Save(run).Error; err != nil {
			log.Errorf("tidy: saving fill run %d: %v", run.ID, err)
		}
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		title, tags, docType, err := s.fillSuggest(ctx, t.DocID, t.AskTags, t.AskType)
		if err != nil && ctx.Err() != nil {
			break // stopped mid-call
		}
		t.Title = title
		if err != nil {
			t.Error = err.Error()
		} else {
			if t.AskTags {
				t.Tags = tags
			}
			if t.AskType {
				t.Type = strings.TrimSpace(docType)
			}
		}
		results = append(results, t)
		save()
	}
	if ctx.Err() != nil {
		run.Status = "stopped"
	} else {
		run.Status = "done"
	}
	save()
	log.Infof("tidy: fill run %d finished: %s, %d of %d documents", run.ID, run.Status, run.Done, run.Total)
}

func (s *tidyService) loadFillRun(id uint) (*TidyFillRun, []tidyFillResult, error) {
	if s.db == nil {
		return nil, nil, tidyRefuse("no database")
	}
	var run TidyFillRun
	if err := s.db.First(&run, id).Error; err != nil {
		return nil, nil, tidyRefuse("no fill run %d", id)
	}
	var results []tidyFillResult
	if run.Results != "" {
		if err := json.Unmarshal([]byte(run.Results), &results); err != nil {
			return nil, nil, err
		}
	}
	return &run, results, nil
}

// fillView resolves a run's suggested names to today's tags and types, the
// way UpdateDocuments would (case-insensitive; names that match nothing are
// dropped and listed), and marks documents that got tags or a type since and
// documents that are on the ignore list.
func (s *tidyService) fillView(ctx context.Context, id uint) (*tidyFillView, error) {
	run, results, err := s.loadFillRun(id)
	if err != nil {
		return nil, err
	}
	tags, err := tidyFetchObjects(ctx, s.client, "tags")
	if err != nil {
		return nil, err
	}
	types, err := tidyFetchObjects(ctx, s.client, "document_types")
	if err != nil {
		return nil, err
	}
	tagIDs, typeIDs := map[string]int{}, map[string]int{}
	for _, t := range tags {
		tagIDs[t.Name] = t.ID
	}
	for _, t := range types {
		typeIDs[t.Name] = t.ID
	}
	tidyApplyAliases(s.db, map[string]int{}, typeIDs)
	ids := make([]int, len(results))
	for i, r := range results {
		ids[i] = r.DocID
	}
	current, err := tidyDocsByIDs(ctx, s.client, ids)
	if err != nil {
		return nil, err
	}
	now := map[int]tidyDoc{}
	for _, d := range current {
		now[d.ID] = d
	}
	ignored, err := s.fillIgnoredIDs()
	if err != nil {
		return nil, err
	}
	view := &tidyFillView{Run: *run, Items: make([]tidyFillItem, 0, len(results))}
	for _, r := range results {
		it := tidyFillItem{DocID: r.DocID, Title: r.Title, AskTags: r.AskTags, AskType: r.AskType, Error: r.Error,
			Ignored: ignored[r.DocID], Tags: []tidyFillTag{}, TagNames: []string{}, TagIDs: []int{}, UnknownTags: []string{}}
		if d, ok := now[r.DocID]; ok {
			it.HasTagsNow, it.HasTypeNow = len(d.Tags) > 0, d.DocumentType != nil
		}
		for _, name := range r.Tags {
			if canonical, id, ok := lookupTagID(tagIDs, name); ok {
				if tidyHasAny(it.TagIDs, []int{id}) {
					continue // the same tag suggested twice
				}
				it.Tags = append(it.Tags, tidyFillTag{ID: id, Name: canonical})
				it.TagNames = append(it.TagNames, canonical)
				it.TagIDs = append(it.TagIDs, id)
			} else if strings.TrimSpace(name) != "" {
				it.UnknownTags = append(it.UnknownTags, name)
			}
		}
		it.TagIDs = tidyUniqueSorted(it.TagIDs)
		if r.Type != "" {
			if canonical, id, ok := lookupNamedID("Document type", typeIDs, r.Type); ok {
				it.TypeName, it.TypeID = canonical, id
			} else {
				it.TypeName = r.Type // shown, but cannot be applied
			}
		}
		view.Items = append(view.Items, it)
	}
	return view, nil
}

// applyFill sets the ticked tags and types on documents that still lack them,
// as one logged action. Documents that got tags or a type since are skipped.
func (s *tidyService) applyFill(ctx context.Context, runID uint, items []tidyFillApplyItem) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	if _, _, err := s.loadFillRun(runID); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, tidyRefuse("nothing ticked")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	ids := make([]int, len(items))
	for i, it := range items {
		ids[i] = it.DocID
	}
	current, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(ids))
	if err != nil {
		return nil, err
	}
	now := map[int]tidyDoc{}
	for _, d := range current {
		now[d.ID] = d
	}
	tagGroups := map[string][]int{} // tag set -> documents
	tagSets := map[string][]int{}
	typeGroups := map[int][]int{}
	skipped := 0
	for _, it := range items {
		d, ok := now[it.DocID]
		if !ok {
			skipped++
			continue
		}
		if set := tidyUniqueSorted(it.TagIDs); len(set) > 0 {
			if len(d.Tags) == 0 {
				key := tidyJoinInts(set)
				tagGroups[key] = append(tagGroups[key], d.ID)
				tagSets[key] = set
			} else {
				skipped++
			}
		}
		if it.TypeID > 0 {
			if d.DocumentType == nil {
				typeGroups[it.TypeID] = append(typeGroups[it.TypeID], d.ID)
			} else {
				skipped++
			}
		}
	}
	if len(tagGroups) == 0 && len(typeGroups) == 0 {
		return nil, tidyRefuse("every ticked document has its tags or type already")
	}
	detail := &tidyActionDetail{FillRun: runID}
	var tagged, typed []int
	for _, docs := range tagGroups {
		tagged = append(tagged, docs...)
	}
	for _, docs := range typeGroups {
		typed = append(typed, docs...)
	}
	for _, id := range tidyUniqueSorted(tagged) {
		detail.Docs = append(detail.Docs, tidyDocChange{ID: id, Before: []int{}})
	}
	for _, id := range tidyUniqueSorted(typed) {
		detail.TypeDocs = append(detail.TypeDocs, tidyDocChange{ID: id, Before: []int{}})
	}
	a := &TidyAction{Action: "fill", Kind: "documents", Status: "started",
		Summary: fmt.Sprintf("Filled tags on %d and a type on %d documents", len(detail.Docs), len(detail.TypeDocs))}
	if skipped > 0 {
		a.Summary += fmt.Sprintf("; skipped %d that have them now", skipped)
	}
	s.saveAction(a, detail)
	fail := func(err error) (*TidyAction, error) {
		a.Status, a.Error = "partial", err.Error()
		s.saveAction(a, detail)
		return a, err
	}
	keys := make([]string, 0, len(tagGroups))
	for k := range tagGroups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := tidyBulkEdit(ctx, s.client, tagGroups[k], "modify_tags", map[string]any{"add_tags": tagSets[k], "remove_tags": []int{}}); err != nil {
			return fail(fmt.Errorf("adding tags: %w", err))
		}
	}
	typeKeys := make([]int, 0, len(typeGroups))
	for k := range typeGroups {
		typeKeys = append(typeKeys, k)
	}
	sort.Ints(typeKeys)
	for _, t := range typeKeys {
		if err := tidySetDocRef(ctx, s.client, "document_types", typeGroups[t], t); err != nil {
			return fail(fmt.Errorf("setting type %d: %w", t, err))
		}
	}
	after, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(append(tagged, typed...)))
	if err != nil {
		return fail(fmt.Errorf("reading documents back: %w", err))
	}
	byID := map[int]tidyDoc{}
	for _, d := range after {
		byID[d.ID] = d
	}
	for i := range detail.Docs {
		detail.Docs[i].After = append([]int{}, byID[detail.Docs[i].ID].Tags...)
	}
	for i := range detail.TypeDocs {
		detail.TypeDocs[i].After = byID[detail.TypeDocs[i].ID].refs("document_types")
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// undoFill takes the filled tags and types off again, where the documents
// still carry exactly what the fill gave them.
func (s *tidyService) undoFill(ctx context.Context, d *tidyActionDetail, force bool) (string, error) {
	ids := []int{}
	for _, ch := range d.Docs {
		ids = append(ids, ch.ID)
	}
	for _, ch := range d.TypeDocs {
		ids = append(ids, ch.ID)
	}
	current, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(ids))
	if err != nil {
		return "", err
	}
	now := map[int]tidyDoc{}
	for _, doc := range current {
		now[doc.ID] = doc
	}
	var untag, untype, drifted []int
	for _, ch := range d.Docs {
		if doc, ok := now[ch.ID]; ok && tidySameSet(doc.Tags, ch.After) {
			untag = append(untag, ch.ID)
		} else {
			drifted = append(drifted, ch.ID)
		}
	}
	for _, ch := range d.TypeDocs {
		if doc, ok := now[ch.ID]; ok && tidySameSet(doc.refs("document_types"), ch.After) {
			untype = append(untype, ch.ID)
		} else {
			drifted = append(drifted, ch.ID)
		}
	}
	drifted = tidyUniqueSorted(drifted)
	if len(drifted) > 0 && !force {
		return "", &tidyRefusal{Message: fmt.Sprintf("%d documents changed since the fill; undo the rest anyway?", len(drifted)), DriftedDocs: drifted}
	}
	for _, id := range untag {
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/documents/%d/", id), map[string]any{"tags": []int{}}, nil); err != nil {
			return "", fmt.Errorf("removing tags from document %d: %w", id, err)
		}
	}
	if len(untype) > 0 {
		if err := tidyBulkEdit(ctx, s.client, untype, "set_document_type", map[string]any{"document_type": nil}); err != nil {
			return "", err
		}
	}
	note := fmt.Sprintf("Took the filled tags off %d and the type off %d documents", len(untag), len(untype))
	if len(drifted) > 0 {
		note += fmt.Sprintf("; left %d that changed since: %s", len(drifted), tidyJoinInts(drifted))
	}
	return note, nil
}
