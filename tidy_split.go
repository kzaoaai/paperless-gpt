package main

// Tidy: split a catch-all correspondent ("Real Estate", "Manufacturer") into
// the real senders. A run asks paperless-gpt's own correspondent prompt about
// every document in the bucket, one at a time in the background, and stores
// each answer. The page groups the answers by suggested correspondent; only
// groups AA ticks are applied, as one logged action that undo reverses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// TidySplitRun is one pass of the correspondent prompt over a bucket.
type TidySplitRun struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	CorrespondentID   int       `json:"correspondent_id"`
	CorrespondentName string    `gorm:"size:255" json:"correspondent_name"`
	ExcludeSelf       bool      `json:"exclude_self"`
	Status            string    `gorm:"size:16" json:"status"` // running, done, stopped, failed
	Total             int       `json:"total"`
	Done              int       `json:"done"`
	Error             string    `gorm:"type:TEXT" json:"error,omitempty"`
	Results           string    `gorm:"type:TEXT" json:"-"`
}

type tidySplitResult struct {
	DocID     int    `json:"doc_id"`
	Title     string `json:"title"`
	Suggested string `json:"suggested"`
	Error     string `json:"error,omitempty"`
}

type tidySplitDoc struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Moved bool   `json:"moved"` // no longer on the bucket
}

type tidySplitGroup struct {
	Key        string         `json:"key"`
	TargetID   int            `json:"target_id,omitempty"`
	TargetName string         `json:"target_name"`
	New        bool           `json:"new"`
	Stay       bool           `json:"stay"`
	Similar    string         `json:"similar,omitempty"` // an existing name a new one looks like
	Docs       []tidySplitDoc `json:"docs"`
}

type tidySplitView struct {
	Run    TidySplitRun      `json:"run"`
	Groups []tidySplitGroup  `json:"groups"`
	Failed []tidySplitResult `json:"failed"`
}

// tidySuggestFunc returns the correspondent paperless-gpt's prompt suggests
// for one document, given the names it may choose from.
type tidySuggestFunc func(ctx context.Context, title, content string, names []string) (string, error)

func (s *tidyService) loadRun(id uint) (*TidySplitRun, []tidySplitResult, error) {
	if s.db == nil {
		return nil, nil, tidyRefuse("no database")
	}
	var run TidySplitRun
	if err := s.db.First(&run, id).Error; err != nil {
		return nil, nil, tidyRefuse("no split run %d", id)
	}
	var results []tidySplitResult
	if run.Results != "" {
		if err := json.Unmarshal([]byte(run.Results), &results); err != nil {
			return nil, nil, err
		}
	}
	return &run, results, nil
}

// startSplit begins a run over every document of a correspondent. One run at
// a time; it reads and asks the LLM but changes nothing.
func (s *tidyService) startSplit(ctx context.Context, corrID int, excludeSelf bool) (*TidySplitRun, error) {
	if s.suggest == nil {
		return nil, tidyRefuse("no LLM is configured")
	}
	if s.db == nil {
		return nil, tidyRefuse("no database")
	}
	s.splitMu.Lock()
	defer s.splitMu.Unlock()
	if s.splitCancel != nil {
		return nil, tidyRefuse("a split run is already going; stop it or wait for it")
	}
	bucket, err := s.mustObject(ctx, "correspondents", corrID)
	if err != nil {
		return nil, err
	}
	docs, err := tidyFetchDocs(ctx, s.client, tidyDocFilter("correspondents", corrID))
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, tidyRefuse("%q has no documents", bucket.Name)
	}
	all, err := tidyFetchObjects(ctx, s.client, "correspondents")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(all))
	for _, o := range all {
		if excludeSelf && o.ID == corrID {
			continue
		}
		names = append(names, o.Name)
	}
	run := &TidySplitRun{CorrespondentID: corrID, CorrespondentName: bucket.Name, ExcludeSelf: excludeSelf,
		Status: "running", Total: len(docs)}
	if err := s.db.Create(run).Error; err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.splitCancel = cancel
	ids := make([]int, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	started := *run // the goroutine keeps updating run
	go s.runSplit(runCtx, run, ids, names)
	return &started, nil
}

func (s *tidyService) stopSplit() bool {
	s.splitMu.Lock()
	defer s.splitMu.Unlock()
	if s.splitCancel == nil {
		return false
	}
	s.splitCancel()
	return true
}

func (s *tidyService) runSplit(ctx context.Context, run *TidySplitRun, ids []int, names []string) {
	defer func() {
		s.splitMu.Lock()
		s.splitCancel = nil
		s.splitMu.Unlock()
	}()
	results := make([]tidySplitResult, 0, len(ids))
	save := func() {
		buf, _ := json.Marshal(results)
		run.Results, run.Done = string(buf), len(results)
		if err := s.db.Save(run).Error; err != nil {
			log.Errorf("tidy: saving split run %d: %v", run.ID, err)
		}
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		var doc struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		}
		res := tidySplitResult{DocID: id}
		if err := tidySend(ctx, s.client, "GET", fmt.Sprintf("api/documents/%d/?fields=id,title,content", id), nil, &doc); err != nil {
			res.Error = err.Error()
		} else {
			res.Title = doc.Title
			name, err := s.suggest(ctx, doc.Title, doc.Content, names)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Suggested = strings.TrimSpace(name)
			}
		}
		if ctx.Err() != nil && res.Error != "" {
			break // stopped mid-call: do not record the cancellation as an answer
		}
		results = append(results, res)
		save()
	}
	switch {
	case ctx.Err() != nil:
		run.Status = "stopped"
	default:
		run.Status = "done"
	}
	save()
	log.Infof("tidy: split run %d over %q finished: %s, %d of %d documents", run.ID, run.CorrespondentName, run.Status, run.Done, run.Total)
}

// splitView groups a run's answers by the correspondent they resolve to,
// the way paperless-gpt would resolve them (case-insensitive, aliases), and
// marks documents that are no longer on the bucket.
func (s *tidyService) splitView(ctx context.Context, id uint) (*tidySplitView, error) {
	run, results, err := s.loadRun(id)
	if err != nil {
		return nil, err
	}
	all, err := tidyFetchObjects(ctx, s.client, "correspondents")
	if err != nil {
		return nil, err
	}
	byName := map[string]int{}
	nameOf := map[int]string{}
	for _, o := range all {
		byName[o.Name] = o.ID
		nameOf[o.ID] = o.Name
	}
	tidyApplyAliases(s.db, byName, map[string]int{})

	ids := make([]int, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.DocID)
	}
	current, err := tidyDocsByIDs(ctx, s.client, ids)
	if err != nil {
		return nil, err
	}
	onBucket := map[int]bool{}
	for _, d := range current {
		onBucket[d.ID] = d.Correspondent != nil && *d.Correspondent == run.CorrespondentID
	}

	view := &tidySplitView{Run: *run, Groups: []tidySplitGroup{}, Failed: []tidySplitResult{}}
	groups := map[string]*tidySplitGroup{}
	for _, r := range results {
		if r.Error != "" {
			view.Failed = append(view.Failed, r)
			continue
		}
		var g *tidySplitGroup
		key, stay := "", false
		targetID, targetName, isNew := 0, r.Suggested, false
		if r.Suggested == "" || isNoMatchCorrespondent(r.Suggested) {
			stay = true
		} else if _, id, ok := lookupNamedID("", byName, r.Suggested); ok {
			if id == run.CorrespondentID {
				stay = true
			} else {
				targetID, targetName = id, nameOf[id]
			}
		} else {
			isNew = true
		}
		switch {
		case stay:
			key, targetName = "stay", run.CorrespondentName
		case isNew:
			key = "new:" + strings.ToLower(targetName)
		default:
			key = fmt.Sprintf("id:%d", targetID)
		}
		if g = groups[key]; g == nil {
			g = &tidySplitGroup{Key: key, TargetID: targetID, TargetName: targetName, New: isNew, Stay: stay}
			if isNew {
				g.Similar = tidySimilarName(targetName, all)
			}
			groups[key] = g
		}
		g.Docs = append(g.Docs, tidySplitDoc{ID: r.DocID, Title: r.Title, Moved: !onBucket[r.DocID]})
	}
	for _, g := range groups {
		view.Groups = append(view.Groups, *g)
	}
	sort.Slice(view.Groups, func(i, j int) bool {
		a, b := view.Groups[i], view.Groups[j]
		if a.Stay != b.Stay {
			return !a.Stay
		}
		if len(a.Docs) != len(b.Docs) {
			return len(a.Docs) > len(b.Docs)
		}
		return strings.ToLower(a.TargetName) < strings.ToLower(b.TargetName)
	})
	return view, nil
}

// tidySimilarName returns an existing correspondent a new name looks like,
// by the same rules as the near-duplicate finding.
func tidySimilarName(name string, existing []tidyObject) string {
	objs := append([]tidyObject{{ID: -1, Name: name}}, existing...)
	for _, g := range tidyNearDuplicates(objs) {
		if g[0] == -1 {
			for _, o := range existing {
				if o.ID == g[1] {
					return o.Name
				}
			}
		}
	}
	return ""
}

type tidySplitApplyGroup struct {
	DocIDs   []int  `json:"doc_ids"`
	TargetID int    `json:"target_id"`
	NewName  string `json:"new_name"`
}

// applySplit moves the chosen documents from the bucket to their groups'
// correspondents, creating new ones where asked, as one logged action.
// Documents no longer on the bucket are skipped.
func (s *tidyService) applySplit(ctx context.Context, runID uint, groups []tidySplitApplyGroup) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	run, _, err := s.loadRun(runID)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, tidyRefuse("nothing ticked")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	bucket, err := s.mustObject(ctx, "correspondents", run.CorrespondentID)
	if err != nil {
		return nil, err
	}
	var all []int
	for _, g := range groups {
		if g.TargetID == run.CorrespondentID {
			return nil, tidyRefuse("a group points back at %q", bucket.Name)
		}
		if g.TargetID == 0 && strings.TrimSpace(g.NewName) == "" {
			return nil, tidyRefuse("a group has no target")
		}
		all = append(all, g.DocIDs...)
	}
	current, err := tidyDocsByIDs(ctx, s.client, tidyUniqueSorted(all))
	if err != nil {
		return nil, err
	}
	still := map[int]bool{}
	for _, d := range current {
		still[d.ID] = d.Correspondent != nil && *d.Correspondent == run.CorrespondentID
	}

	detail := &tidyActionDetail{Object: bucket.Raw, SplitRun: run.ID}
	a := &TidyAction{Action: "split", Kind: "correspondents", ObjectID: bucket.ID, ObjectName: bucket.Name, Status: "started"}
	s.saveAction(a, detail)
	fail := func(err error) (*TidyAction, error) {
		a.Status, a.Error = "partial", err.Error()
		if len(detail.Docs) == 0 && len(detail.Created) == 0 {
			a.Status = "failed"
		}
		s.saveAction(a, detail)
		return a, err
	}

	var viewer *tidyUser
	if s.viewer != "" {
		viewer, _ = tidyFetchUser(ctx, s.client, s.viewer)
	}
	moved, targets := 0, 0
	var skipped []int
	for _, g := range groups {
		var ids []int
		for _, id := range tidyUniqueSorted(g.DocIDs) {
			if still[id] {
				ids = append(ids, id)
			} else {
				skipped = append(skipped, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		target := g.TargetID
		if target == 0 {
			name := strings.TrimSpace(g.NewName)
			var page struct {
				Results []tidyObject `json:"results"`
			}
			if err := tidySend(ctx, s.client, "GET", "api/correspondents/?name__iexact="+url.QueryEscape(name), nil, &page); err != nil {
				return fail(err)
			}
			if len(page.Results) > 0 {
				target = page.Results[0].ID
			} else {
				// Created the way paperless-gpt creates correspondents, so the
				// owner is the API user; then visible to the viewer as well.
				var created tidyObject
				if err := tidySend(ctx, s.client, "POST", "api/correspondents/", instantiateCorrespondent(name), &created); err != nil {
					return fail(fmt.Errorf("creating %q: %w", name, err))
				}
				target = created.ID
				detail.Created = append(detail.Created, target)
				s.saveAction(a, detail)
				if viewer != nil {
					body := map[string]any{"objects": []int{target}, "object_type": "correspondents", "operation": "set_permissions", "merge": true,
						"permissions": tidyPerms{View: tidyPermSet{Users: []int{viewer.ID}, Groups: []int{}}, Change: tidyPermSet{Users: []int{}, Groups: []int{}}}}
					if err := tidySend(ctx, s.client, "POST", "api/bulk_edit_objects/", body, nil); err != nil {
						log.Warnf("tidy: granting %s view on new correspondent %q: %v", viewer.Username, name, err)
					}
				}
			}
		}
		if err := tidySetDocRef(ctx, s.client, "correspondents", ids, target); err != nil {
			return fail(fmt.Errorf("moving documents to %d: %w", target, err))
		}
		for _, id := range ids {
			detail.Docs = append(detail.Docs, tidyDocChange{ID: id, Before: []int{bucket.ID}, After: []int{target}})
		}
		moved += len(ids)
		targets++
		s.saveAction(a, detail)
	}
	a.Summary = fmt.Sprintf("Split %d documents out of %q into %d correspondents (%d new)", moved, bucket.Name, targets, len(detail.Created))
	if len(skipped) > 0 {
		a.Summary += fmt.Sprintf("; skipped %d no longer on it", len(skipped))
	}
	if moved == 0 {
		a.Status, a.Error = "failed", "no ticked document is still on "+bucket.Name
		s.saveAction(a, detail)
		return a, tidyRefuse("%s", a.Error)
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// undoSplit moves the split's documents back to the bucket and deletes the
// correspondents it created once they are empty again.
func (s *tidyService) undoSplit(ctx context.Context, a *TidyAction, d *tidyActionDetail, force bool) (string, error) {
	ids := make([]int, 0, len(d.Docs))
	for _, ch := range d.Docs {
		ids = append(ids, ch.ID)
	}
	current, err := tidyDocsByIDs(ctx, s.client, ids)
	if err != nil {
		return "", err
	}
	byID := map[int]tidyDoc{}
	for _, doc := range current {
		byID[doc.ID] = doc
	}
	var back, drifted []int
	for _, ch := range d.Docs {
		doc, ok := byID[ch.ID]
		if !ok || !tidySameSet(doc.refs("correspondents"), ch.After) {
			drifted = append(drifted, ch.ID)
			continue
		}
		back = append(back, ch.ID)
	}
	sort.Ints(drifted)
	if len(drifted) > 0 && !force {
		return "", &tidyRefusal{Message: fmt.Sprintf("%d documents changed since the split; undo the rest anyway?", len(drifted)), DriftedDocs: drifted}
	}
	bucketID, err := s.recreate(ctx, "correspondents", d.Object)
	if err != nil {
		return "", err
	}
	if len(back) > 0 {
		if err := tidySetDocRef(ctx, s.client, "correspondents", back, bucketID); err != nil {
			return "", err
		}
	}
	var removed, kept []int
	for _, id := range d.Created {
		left, err := tidyFetchDocs(ctx, s.client, tidyDocFilter("correspondents", id))
		if err != nil {
			return "", err
		}
		obj, err := tidyFetchObject(ctx, s.client, "correspondents", id)
		if err != nil {
			return "", err
		}
		if obj == nil {
			continue
		}
		refs, _, refErr := s.objectRefs(ctx, "correspondents", *obj)
		if len(left) > 0 || refErr != nil || len(refs) > 0 {
			kept = append(kept, id)
			continue
		}
		if err := tidySend(ctx, s.client, "DELETE", fmt.Sprintf("api/correspondents/%d/", id), nil, nil); err != nil {
			var apiErr *tidyAPIError
			if !errors.As(err, &apiErr) || apiErr.Status != 404 {
				return "", err
			}
		}
		removed = append(removed, id)
	}
	note := fmt.Sprintf("Moved %d documents back to %q", len(back), a.ObjectName)
	if len(removed) > 0 {
		note += fmt.Sprintf("; deleted %d correspondents the split created", len(removed))
	}
	if len(kept) > 0 {
		note += fmt.Sprintf("; kept %d it created that are in use now: %s", len(kept), tidyJoinInts(kept))
	}
	if len(drifted) > 0 {
		note += fmt.Sprintf("; left %d that changed since: %s", len(drifted), tidyJoinInts(drifted))
	}
	return note, nil
}
