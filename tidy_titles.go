package main

// Tidy: re-check titles. A background run asks paperless-gpt's own title
// prompt (nothing else) about documents whose title looks like an upload file
// name, a random ID or a scanner default, and stores the answers. The page
// shows the current title beside the suggestion, which can be edited; only
// selected rows are applied, as one logged action that undo reverses. A
// document on the title ignore list is not asked about again until it is taken
// off it. The list is separate from Fill's.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// paperless-ngx keeps titles to 128 characters.
const tidyTitleMax = 128

// TidyTitleRun is one pass of the title prompt.
type TidyTitleRun struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Status    string    `gorm:"size:16" json:"status"` // running, done, stopped
	Total     int       `json:"total"`
	Done      int       `json:"done"`
	Results   string    `gorm:"type:TEXT" json:"-"`
}

// TidyTitleIgnore is a document whose title a person chose to keep.
type TidyTitleIgnore struct {
	DocID     int       `gorm:"primaryKey;autoIncrement:false" json:"doc_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

type tidyTitleResult struct {
	DocID     int    `json:"doc_id"`
	Title     string `json:"title"` // the title when the run asked
	Suggested string `json:"suggested"`
	Error     string `json:"error,omitempty"`
}

// tidyTitleSuggestFunc runs paperless-gpt's title prompt on one document and
// returns its current title and the suggestion.
type tidyTitleSuggestFunc func(ctx context.Context, docID int) (title, suggested string, err error)

type tidyTitleItem struct {
	DocID     int    `json:"doc_id"`
	Title     string `json:"title"`   // when the run asked
	Current   string `json:"current"` // now
	Suggested string `json:"suggested"`
	// Changed: the title is no longer what the run saw (applied, or edited by hand).
	Changed bool   `json:"changed"`
	Gone    bool   `json:"gone"`
	Ignored bool   `json:"ignored"`
	Error   string `json:"error,omitempty"`
}

type tidyTitleView struct {
	Run   TidyTitleRun    `json:"run"`
	Items []tidyTitleItem `json:"items"`
}

type tidyTitleIgnored struct {
	TidyTitleIgnore
	Current string `json:"current"`
	Gone    bool   `json:"gone"`
}

type tidyTitleStatus struct {
	Candidates int                `json:"candidates"` // titles that look raw, not ignored or queued
	Documents  int                `json:"documents"`
	Ignored    []tidyTitleIgnored `json:"ignored"`
}

type tidyTitleApplyItem struct {
	DocID int    `json:"doc_id"`
	Title string `json:"title"`
}

type tidyTitleChange struct {
	ID     int    `json:"id"`
	Before string `json:"before"`
	After  string `json:"after"`
}

var (
	tidyUUIDRe  = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	tidyExtRe   = regexp.MustCompile(`(?i)\.(pdf|jpe?g|png|heic|tiff?|docx?|xlsx?|eml|cgi|txt)$`)
	tidyTokenRe = regexp.MustCompile(`^[\w.\-]+$`)
	tidyDigitRe = regexp.MustCompile(`^[\d\s\-_.]+$`)
	tidyGeneric = map[string]bool{"pdf document": true, "scan": true, "document": true, "untitled": true,
		"image": true, "receipt": true, "invoice": true, "statement": true, "summary": true, "scanned document": true}
)

// tidyTitleLooksRaw says whether a title is what an upload or scanner gave the
// document rather than a title: its file name, a random ID, a file extension,
// an underscore or single-word name, digits only, or a generic label. A file
// name a person chose may still read well; the page shows both so they decide.
func tidyTitleLooksRaw(title, fileName string) bool {
	t := strings.TrimSpace(title)
	if t == "" {
		return true
	}
	base := strings.TrimSpace(tidyExtRe.ReplaceAllString(strings.TrimSpace(fileName), ""))
	switch {
	case base != "" && strings.EqualFold(t, base):
		return true
	case tidyUUIDRe.MatchString(t), tidyExtRe.MatchString(t), tidyDigitRe.MatchString(t):
		return true
	case tidyGeneric[strings.ToLower(t)]:
		return true
	case !strings.Contains(t, " ") && (strings.Contains(t, "_") || tidyTokenRe.MatchString(t)):
		return true
	}
	return false
}

// tidyTitleTargets picks the documents to ask about: explicit ids when given
// (ignored ones too, since a person asked for them), otherwise every document
// whose title looks raw, not ignored and not waiting in one of paperless-gpt's
// queues. Newest first, at most limit when limit > 0.
func tidyTitleTargets(docs []tidyDoc, queueTagIDs []int, ignored map[int]bool, ids []int, limit int) []tidyTitleResult {
	want := map[int]bool{}
	for _, id := range ids {
		want[id] = true
	}
	sorted := append([]tidyDoc(nil), docs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Added > sorted[j].Added })
	var out []tidyTitleResult
	for _, d := range sorted {
		if len(want) > 0 {
			if !want[d.ID] {
				continue
			}
		} else if ignored[d.ID] || tidyHasAny(d.Tags, queueTagIDs) || !tidyTitleLooksRaw(d.Title, d.OriginalFileName) {
			continue
		}
		out = append(out, tidyTitleResult{DocID: d.ID, Title: d.Title})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (s *tidyService) titleIgnoredIDs() (map[int]bool, error) {
	out := map[int]bool{}
	if s.db == nil {
		return out, nil
	}
	var ids []int
	if err := s.db.Model(&TidyTitleIgnore{}).Pluck("doc_id", &ids).Error; err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

func (s *tidyService) titleTargets(ctx context.Context, ids []int, limit int) ([]tidyTitleResult, []tidyDoc, error) {
	docs, err := tidyFetchDocs(ctx, s.client, "")
	if err != nil {
		return nil, nil, err
	}
	tags, err := tidyFetchObjects(ctx, s.client, "tags")
	if err != nil {
		return nil, nil, err
	}
	ignored, err := s.titleIgnoredIDs()
	if err != nil {
		return nil, nil, err
	}
	cfg := s.configRefs()
	queue := append(tidyTagIDsByName(tags, cfg.AutoTag), tidyTagIDsByName(tags, cfg.OCRTag)...)
	return tidyTitleTargets(docs, queue, ignored, ids, limit), docs, nil
}

// titleStatus counts the candidates and lists the ignored documents with their
// title today.
func (s *tidyService) titleStatus(ctx context.Context) (*tidyTitleStatus, error) {
	targets, docs, err := s.titleTargets(ctx, nil, 0)
	if err != nil {
		return nil, err
	}
	st := &tidyTitleStatus{Candidates: len(targets), Documents: len(docs), Ignored: []tidyTitleIgnored{}}
	if s.db != nil {
		var rows []TidyTitleIgnore
		if err := s.db.Order("created_at desc, doc_id desc").Find(&rows).Error; err != nil {
			return nil, err
		}
		byID := make(map[int]tidyDoc, len(docs))
		for _, d := range docs {
			byID[d.ID] = d
		}
		for _, r := range rows {
			it := tidyTitleIgnored{TidyTitleIgnore: r}
			if d, ok := byID[r.DocID]; ok {
				it.Current = d.Title
			} else {
				it.Gone = true
			}
			st.Ignored = append(st.Ignored, it)
		}
	}
	return st, nil
}

func (s *tidyService) ignoreTitles(items []tidyFillIgnoreItem) (int, error) {
	if s.db == nil {
		return 0, tidyRefuse("no database")
	}
	added := 0
	for _, it := range items {
		if it.DocID <= 0 {
			continue
		}
		row := TidyTitleIgnore{DocID: it.DocID, Title: strings.TrimSpace(it.Title)}
		res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			return added, res.Error
		}
		added += int(res.RowsAffected)
	}
	return added, nil
}

func (s *tidyService) unignoreTitles(ids []int) (int, error) {
	if s.db == nil {
		return 0, tidyRefuse("no database")
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := s.db.Where("doc_id IN ?", ids).Delete(&TidyTitleIgnore{})
	return int(res.RowsAffected), res.Error
}

func (s *tidyService) startTitles(ctx context.Context, ids []int, limit int) (*TidyTitleRun, error) {
	if s.titleSuggest == nil {
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
	targets, _, err := s.titleTargets(ctx, ids, limit)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, tidyRefuse("nothing to ask about: no title looks like a file name, or those that do are ignored")
	}
	run := &TidyTitleRun{Status: "running", Total: len(targets)}
	if err := s.db.Create(run).Error; err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.splitCancel = cancel
	started := *run
	go s.runTitles(runCtx, run, targets)
	return &started, nil
}

func (s *tidyService) runTitles(ctx context.Context, run *TidyTitleRun, targets []tidyTitleResult) {
	defer func() {
		s.splitMu.Lock()
		s.splitCancel = nil
		s.splitMu.Unlock()
	}()
	results := make([]tidyTitleResult, 0, len(targets))
	save := func() {
		buf, _ := json.Marshal(results)
		run.Results, run.Done = string(buf), len(results)
		if err := s.db.Save(run).Error; err != nil {
			log.Errorf("tidy: saving title run %d: %v", run.ID, err)
		}
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		title, suggested, err := s.titleSuggest(ctx, t.DocID)
		if err != nil && ctx.Err() != nil {
			break // stopped mid-call
		}
		if title != "" {
			t.Title = title
		}
		if err != nil {
			t.Error = err.Error()
		} else {
			t.Suggested = tidyCleanTitle(suggested)
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
	log.Infof("tidy: title run %d finished: %s, %d of %d documents", run.ID, run.Status, run.Done, run.Total)
}

// tidyCleanTitle trims a title and cuts it to what paperless-ngx keeps.
func tidyCleanTitle(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > tidyTitleMax {
		t = strings.TrimSpace(string(r[:tidyTitleMax]))
	}
	return t
}

func (s *tidyService) loadTitleRun(id uint) (*TidyTitleRun, []tidyTitleResult, error) {
	if s.db == nil {
		return nil, nil, tidyRefuse("no database")
	}
	var run TidyTitleRun
	if err := s.db.First(&run, id).Error; err != nil {
		return nil, nil, tidyRefuse("no title run %d", id)
	}
	var results []tidyTitleResult
	if run.Results != "" {
		if err := json.Unmarshal([]byte(run.Results), &results); err != nil {
			return nil, nil, err
		}
	}
	return &run, results, nil
}

// titleView adds each document's title today, so the page can tell what was
// applied or edited since the run.
func (s *tidyService) titleView(ctx context.Context, id uint) (*tidyTitleView, error) {
	run, results, err := s.loadTitleRun(id)
	if err != nil {
		return nil, err
	}
	ids := make([]int, len(results))
	for i, r := range results {
		ids[i] = r.DocID
	}
	current, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(ids))
	if err != nil {
		return nil, err
	}
	now := map[int]tidyDoc{}
	for _, d := range current {
		now[d.ID] = d
	}
	ignored, err := s.titleIgnoredIDs()
	if err != nil {
		return nil, err
	}
	view := &tidyTitleView{Run: *run, Items: make([]tidyTitleItem, 0, len(results))}
	for _, r := range results {
		it := tidyTitleItem{DocID: r.DocID, Title: r.Title, Suggested: r.Suggested, Ignored: ignored[r.DocID], Error: r.Error}
		if d, ok := now[r.DocID]; ok {
			it.Current = d.Title
			it.Changed = d.Title != r.Title
		} else {
			it.Gone = true
		}
		view.Items = append(view.Items, it)
	}
	return view, nil
}

// applyTitles sets the given titles, as one logged action. A document whose
// title changed since the run is skipped, so a hand edit is never overwritten.
func (s *tidyService) applyTitles(ctx context.Context, runID uint, items []tidyTitleApplyItem) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	_, results, err := s.loadTitleRun(runID)
	if err != nil {
		return nil, err
	}
	asked := map[int]string{}
	for _, r := range results {
		asked[r.DocID] = r.Title
	}
	if len(items) == 0 {
		return nil, tidyRefuse("nothing selected")
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
	detail := &tidyActionDetail{TitleRun: runID}
	skipped := 0
	seen := map[int]bool{}
	for _, it := range items {
		title := tidyCleanTitle(it.Title)
		d, ok := now[it.DocID]
		before, inRun := asked[it.DocID]
		if !ok || !inRun || seen[it.DocID] || title == "" || d.Title != before || title == d.Title {
			skipped++
			continue
		}
		seen[it.DocID] = true
		detail.Titles = append(detail.Titles, tidyTitleChange{ID: d.ID, Before: d.Title, After: title})
	}
	if len(detail.Titles) == 0 {
		return nil, tidyRefuse("no selected title can be applied: each is empty, unchanged, or was changed since the run")
	}
	a := &TidyAction{Action: "titles", Kind: "documents", Status: "started",
		Summary: fmt.Sprintf("Retitled %d documents", len(detail.Titles))}
	if skipped > 0 {
		a.Summary += fmt.Sprintf("; skipped %d (empty, unchanged, or edited since the run)", skipped)
	}
	s.saveAction(a, detail)
	for i, ch := range detail.Titles {
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/documents/%d/", ch.ID), map[string]any{"title": ch.After}, nil); err != nil {
			detail.Titles = detail.Titles[:i] // only the ones that were set, so undo touches nothing else
			a.Status, a.Error = "partial", fmt.Sprintf("setting the title of document %d: %v", ch.ID, err)
			s.saveAction(a, detail)
			return a, err
		}
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// undoTitles puts the old titles back where the documents still carry the
// applied one.
func (s *tidyService) undoTitles(ctx context.Context, d *tidyActionDetail, force bool) (string, error) {
	ids := make([]int, len(d.Titles))
	for i, ch := range d.Titles {
		ids[i] = ch.ID
	}
	current, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(ids))
	if err != nil {
		return "", err
	}
	now := map[int]tidyDoc{}
	for _, doc := range current {
		now[doc.ID] = doc
	}
	var back []tidyTitleChange
	var drifted []int
	for _, ch := range d.Titles {
		if doc, ok := now[ch.ID]; ok && doc.Title == ch.After {
			back = append(back, ch)
		} else {
			drifted = append(drifted, ch.ID)
		}
	}
	drifted = tidyUniqueSorted(drifted)
	if len(drifted) > 0 && !force {
		return "", &tidyRefusal{Message: fmt.Sprintf("%d documents changed since; undo the rest anyway?", len(drifted)), DriftedDocs: drifted}
	}
	for _, ch := range back {
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/documents/%d/", ch.ID), map[string]any{"title": ch.Before}, nil); err != nil {
			return "", fmt.Errorf("restoring the title of document %d: %w", ch.ID, err)
		}
	}
	note := fmt.Sprintf("Put the old title back on %d documents", len(back))
	if len(drifted) > 0 {
		note += fmt.Sprintf("; left %d that changed since: %s", len(drifted), tidyJoinInts(drifted))
	}
	return note, nil
}
