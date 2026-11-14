package main

// Tidy: the write actions (merge, rename, delete, matching and permission
// fixes), their log, and undo. Every action is logged before it changes
// anything, with enough to reverse it: the removed object's full JSON
// (matching rules, owner, permissions) and each moved document's before and
// after state. Undo refuses documents that changed since, unless forced.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// TidyAction is one logged Tidy action.
type TidyAction struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	Action     string     `gorm:"size:32;not null" json:"action"` // merge, rename, delete, set_matching, grant_viewer
	Kind       string     `gorm:"size:32;not null" json:"kind"`
	ObjectID   int        `json:"object_id"`
	ObjectName string     `gorm:"size:255" json:"object_name"`
	TargetID   int        `json:"target_id,omitempty"`
	TargetName string     `gorm:"size:255" json:"target_name,omitempty"`
	Summary    string     `gorm:"size:1024" json:"summary"`
	Status     string     `gorm:"size:16" json:"status"` // started, done, partial, failed
	Error      string     `gorm:"type:TEXT" json:"error,omitempty"`
	Detail     string     `gorm:"type:TEXT" json:"-"`
	Undone     bool       `json:"undone"`
	UndoneAt   *time.Time `json:"undone_at,omitempty"`
	UndoNote   string     `gorm:"type:TEXT" json:"undo_note,omitempty"`
}

// TidyAlias maps a name that was merged away to the object it was merged
// into. paperless-gpt resolves LLM-suggested correspondents and document
// types through it, so a merged-away name is not created again.
type TidyAlias struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Kind      string    `gorm:"size:32;not null;index" json:"kind"`
	Name      string    `gorm:"size:255;not null" json:"name"`
	TargetID  int       `json:"target_id"`
	ActionID  uint      `json:"action_id"`
}

type tidyDocChange struct {
	ID     int   `json:"id"`
	Before []int `json:"before"`
	After  []int `json:"after,omitempty"`
}

type tidyObjectState struct {
	ID  int             `json:"id"`
	Raw json.RawMessage `json:"raw"`
}

type tidyActionDetail struct {
	Object      json.RawMessage   `json:"object,omitempty"`
	Docs        []tidyDocChange   `json:"docs,omitempty"`
	Children    []int             `json:"children,omitempty"`
	AliasID     uint              `json:"alias_id,omitempty"`
	OldName     string            `json:"old_name,omitempty"`
	NewName     string            `json:"new_name,omitempty"`
	Objects     []tidyObjectState `json:"objects,omitempty"`
	Set         map[string]any    `json:"set,omitempty"`
	RecreatedID int               `json:"recreated_id,omitempty"`
	// Split: correspondents the split created, and the run it came from.
	Created  []int `json:"created,omitempty"`
	SplitRun uint  `json:"split_run,omitempty"`
}

// tidyRefusal is an action Tidy will not take; the page shows why.
type tidyRefusal struct {
	Message     string    `json:"error"`
	References  []tidyRef `json:"references,omitempty"`
	DriftedDocs []int     `json:"drifted_docs,omitempty"`
}

func (r *tidyRefusal) Error() string { return r.Message }

func tidyRefuse(format string, args ...any) *tidyRefusal {
	return &tidyRefusal{Message: fmt.Sprintf(format, args...)}
}

// tidyService holds what the Tidy handlers share.
type tidyService struct {
	client     tidyHTTP
	db         *gorm.DB
	llm        func(ctx context.Context, prompt string) (string, error)
	writes     bool
	viewer     string
	configRefs func() tidyConfigRefs

	mu      sync.Mutex // serializes write actions and undo
	cacheMu sync.Mutex
	cache   *tidyInventory

	suggest     tidySuggestFunc // paperless-gpt's correspondent prompt; nil without an LLM
	splitMu     sync.Mutex
	splitCancel context.CancelFunc // set while a split run is going
}

func (s *tidyService) invalidate() {
	s.cacheMu.Lock()
	s.cache = nil
	s.cacheMu.Unlock()
}

// inventory returns the cached inventory, rebuilding it when asked or absent.
func (s *tidyService) inventory(ctx context.Context, refresh bool) (*tidyInventory, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache != nil && !refresh {
		return s.cache, nil
	}
	objects := map[string][]tidyObject{}
	for _, kind := range tidyKinds {
		objs, err := tidyFetchObjects(ctx, s.client, kind)
		if err != nil {
			return nil, err
		}
		objects[kind] = objs
	}
	docs, err := tidyFetchDocs(ctx, s.client, "")
	if err != nil {
		return nil, err
	}
	refs := tidyFetchReferences(ctx, s.client)
	allRefs := append(refs.Refs, tidyConfigReferences(s.configRefs(), objects["tags"])...)
	inv := &tidyInventory{GeneratedAt: time.Now(), Writes: s.writes, Viewer: s.viewer, DocumentCount: len(docs),
		UpdatedWorkflows: refs.Updated, Warnings: refs.Warnings}
	var viewer *tidyUser
	if s.viewer != "" {
		viewer, err = tidyFetchUser(ctx, s.client, s.viewer)
		if err != nil {
			inv.Warnings = append(inv.Warnings, "Could not look up "+s.viewer+": "+err.Error())
		} else if viewer == nil {
			inv.Warnings = append(inv.Warnings, "No paperless-ngx user named "+s.viewer)
		} else {
			inv.ViewerFound, inv.ViewerID = true, viewer.ID
		}
	}
	inv.Objects, inv.Findings, inv.MatchingSummary = tidyBuildInventory(objects, docs, allRefs, viewer, tidyFetchUsers(ctx, s.client))
	inv.Hygiene = tidyBuildHygiene(docs, objects["tags"], s.configRefs(), time.Now())
	inv.FieldAudit = tidyBuildFieldAudit(docs, objects["custom_fields"])
	inv.Aliases = []TidyAlias{}
	if s.db != nil {
		s.db.Order("id").Find(&inv.Aliases)
	}
	s.cache = inv
	return inv, nil
}

// objectRefs returns what refers to obj right now. It fails rather than
// report "nothing" when paperless-ngx's references could not be read.
func (s *tidyService) objectRefs(ctx context.Context, kind string, obj tidyObject) ([]tidyRef, []tidyWorkflowInfo, error) {
	refs := tidyFetchReferences(ctx, s.client)
	if len(refs.Warnings) > 0 {
		return nil, nil, errors.New(strings.Join(refs.Warnings, "; "))
	}
	// Tag names in paperless-gpt's config resolve against this object only;
	// tidyRefsFor keeps references of this kind and id.
	all := append(refs.Refs, tidyConfigReferences(s.configRefs(), []tidyObject{obj})...)
	return tidyRefsFor(all, kind, obj), refs.Updated, nil
}

func (s *tidyService) saveAction(a *TidyAction, d *tidyActionDetail) {
	if d != nil {
		if buf, err := json.Marshal(d); err == nil {
			a.Detail = string(buf)
		}
	}
	if s.db == nil {
		return
	}
	if err := s.db.Save(a).Error; err != nil {
		log.Errorf("tidy: saving action log: %v", err)
	}
}

func (s *tidyService) requireWrites() error {
	if !s.writes {
		return tidyRefuse("Tidy is read-only: set TIDY_WRITES=true to allow changes")
	}
	return nil
}

func (s *tidyService) mustObject(ctx context.Context, kind string, id int) (*tidyObject, error) {
	obj, err := tidyFetchObject(ctx, s.client, kind, id)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, tidyRefuse("%s %d does not exist", strings.TrimSuffix(kind, "s"), id)
	}
	return obj, nil
}

// --- merge ---

type tidyMergePlan struct {
	Kind             string             `json:"kind"`
	Source           tidyRow            `json:"source"`
	Target           tidyRow            `json:"target"`
	Docs             []int              `json:"docs"`
	Children         []int              `json:"children,omitempty"`
	References       []tidyRef          `json:"references"`
	UpdatedWorkflows []tidyWorkflowInfo `json:"updated_workflows"`
	Alias            string             `json:"alias,omitempty"`
	Blocked          string             `json:"blocked,omitempty"`
}

// tidyDescendants returns every tag below id.
func tidyDescendants(tags []tidyObject, id int) map[int]bool {
	children := map[int][]int{}
	for _, t := range tags {
		if t.Parent != nil {
			children[*t.Parent] = append(children[*t.Parent], t.ID)
		}
	}
	out := map[int]bool{}
	queue := []int{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if !out[c] {
				out[c] = true
				queue = append(queue, c)
			}
		}
	}
	return out
}

// planMerge reads everything a merge of source into target would touch and
// says whether it is blocked.
func (s *tidyService) planMerge(ctx context.Context, kind string, sourceID, targetID int) (*tidyMergePlan, *tidyObject, error) {
	if !tidyMergeable(kind) {
		return nil, nil, tidyRefuse("%s cannot be merged", kind)
	}
	if sourceID == targetID {
		return nil, nil, tidyRefuse("pick two different objects")
	}
	src, err := s.mustObject(ctx, kind, sourceID)
	if err != nil {
		return nil, nil, err
	}
	dst, err := s.mustObject(ctx, kind, targetID)
	if err != nil {
		return nil, nil, err
	}
	plan := &tidyMergePlan{Kind: kind, Source: tidyRow{ID: src.ID, Name: src.Name}, Target: tidyRow{ID: dst.ID, Name: dst.Name}}
	refs, updated, err := s.objectRefs(ctx, kind, *src)
	if err != nil {
		return nil, nil, err
	}
	plan.References, plan.UpdatedWorkflows = refs, updated
	docs, err := tidyFetchDocs(ctx, s.client, tidyDocFilter(kind, src.ID))
	if err != nil {
		return nil, nil, err
	}
	for _, d := range docs {
		plan.Docs = append(plan.Docs, d.ID)
	}
	plan.Source.DocCount, plan.Target.DocCount = len(docs), -1
	if kind == "tags" {
		tags, err := tidyFetchObjects(ctx, s.client, "tags")
		if err != nil {
			return nil, nil, err
		}
		for _, t := range tags {
			if t.Parent != nil && *t.Parent == src.ID {
				plan.Children = append(plan.Children, t.ID)
			}
		}
		if tidyDescendants(tags, src.ID)[dst.ID] {
			plan.Blocked = fmt.Sprintf("%q is below %q; merge it the other way round, or move it out first", dst.Name, src.Name)
		}
	}
	if kind == "correspondents" || kind == "document_types" {
		plan.Alias = src.Name
	}
	if len(refs) > 0 && plan.Blocked == "" {
		plan.Blocked = fmt.Sprintf("%q is still used by %d workflow, view, mail rule or paperless-gpt setting(s); change those first", src.Name, len(refs))
	}
	return plan, src, nil
}

// merge moves every document from source to target, deletes source once it
// has no documents left, and records an alias for the old name.
func (s *tidyService) merge(ctx context.Context, kind string, sourceID, targetID int) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	plan, src, err := s.planMerge(ctx, kind, sourceID, targetID)
	if err != nil {
		return nil, err
	}
	if plan.Blocked != "" {
		return nil, &tidyRefusal{Message: plan.Blocked, References: plan.References}
	}

	before, err := tidyDocsByIDs(ctx, s.client, plan.Docs)
	if err != nil {
		return nil, err
	}
	detail := &tidyActionDetail{Object: src.Raw, Children: plan.Children}
	for _, d := range before {
		ch := tidyDocChange{ID: d.ID, Before: d.refs(kind)}
		detail.Docs = append(detail.Docs, ch)
	}
	a := &TidyAction{Action: "merge", Kind: kind, ObjectID: src.ID, ObjectName: src.Name, TargetID: plan.Target.ID,
		TargetName: plan.Target.Name, Status: "started",
		Summary: fmt.Sprintf("Merge %q into %q (%d documents)", src.Name, plan.Target.Name, len(plan.Docs))}
	s.saveAction(a, detail)

	fail := func(status string, err error) (*TidyAction, error) {
		a.Status, a.Error = status, err.Error()
		s.saveAction(a, detail)
		return a, err
	}

	// Tags: move the children first. Removing a tag from documents also
	// removes its descendants, so children still under source would be
	// stripped from those documents.
	for i, child := range plan.Children {
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/tags/%d/", child), map[string]any{"parent": targetID}, nil); err != nil {
			status := "failed"
			if i > 0 {
				status = "partial"
			}
			return fail(status, fmt.Errorf("moving child tag %d under %q: %w", child, plan.Target.Name, err))
		}
	}

	if len(plan.Docs) > 0 {
		if kind == "tags" {
			err = tidyBulkEdit(ctx, s.client, plan.Docs, "modify_tags", map[string]any{"add_tags": []int{targetID}, "remove_tags": []int{sourceID}})
		} else {
			err = tidySetDocRef(ctx, s.client, kind, plan.Docs, targetID)
		}
		if err != nil {
			return fail("partial", fmt.Errorf("moving documents: %w", err))
		}
		after, err := tidyDocsByIDs(ctx, s.client, plan.Docs)
		if err != nil {
			return fail("partial", fmt.Errorf("reading documents back: %w", err))
		}
		byID := map[int]tidyDoc{}
		for _, d := range after {
			byID[d.ID] = d
		}
		for i := range detail.Docs {
			if d, ok := byID[detail.Docs[i].ID]; ok {
				detail.Docs[i].After = d.refs(kind)
			}
		}
		s.saveAction(a, detail)
	}

	left, err := tidyFetchDocs(ctx, s.client, tidyDocFilter(kind, sourceID))
	if err != nil {
		return fail("partial", fmt.Errorf("re-checking %q: %w", src.Name, err))
	}
	if len(left) > 0 {
		return fail("partial", fmt.Errorf("%d documents still use %q, so it was not deleted", len(left), src.Name))
	}
	if err := tidySend(ctx, s.client, "DELETE", fmt.Sprintf("api/%s/%d/", kind, sourceID), nil, nil); err != nil {
		return fail("partial", fmt.Errorf("deleting %q: %w", src.Name, err))
	}

	if plan.Alias != "" && s.db != nil {
		alias := &TidyAlias{Kind: kind, Name: plan.Alias, TargetID: targetID, ActionID: a.ID}
		var existing []TidyAlias
		s.db.Where("kind = ? AND lower(name) = lower(?)", kind, plan.Alias).Limit(1).Find(&existing)
		if len(existing) > 0 {
			existing[0].TargetID, existing[0].ActionID = targetID, a.ID
			alias = &existing[0]
		}
		if err := s.db.Save(alias).Error; err != nil {
			log.Errorf("tidy: saving alias %q: %v", plan.Alias, err)
		} else {
			detail.AliasID = alias.ID
		}
		// Earlier aliases that pointed at the deleted source now point at
		// the target.
		s.db.Model(&TidyAlias{}).Where("kind = ? AND target_id = ?", kind, sourceID).Update("target_id", targetID)
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// --- rename and delete ---

func (s *tidyService) rename(ctx context.Context, kind string, id int, name string) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, tidyRefuse("the new name is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	obj, err := s.mustObject(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if obj.Name == name {
		return nil, tidyRefuse("%q already has that name", obj.Name)
	}
	if kind == "tags" {
		for n, where := range s.configRefs().TagNames {
			if strings.EqualFold(n, obj.Name) {
				return nil, &tidyRefusal{Message: fmt.Sprintf("paperless-gpt refers to %q by name (%s); renaming it would break that", obj.Name, where)}
			}
		}
	}
	detail := &tidyActionDetail{Object: obj.Raw, OldName: obj.Name, NewName: name}
	a := &TidyAction{Action: "rename", Kind: kind, ObjectID: id, ObjectName: obj.Name, Status: "started",
		Summary: fmt.Sprintf("Rename %q to %q", obj.Name, name)}
	s.saveAction(a, detail)
	if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/%s/%d/", kind, id), map[string]any{"name": name}, nil); err != nil {
		a.Status, a.Error = "failed", err.Error()
		s.saveAction(a, detail)
		return a, err
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

func (s *tidyService) deleteEmpty(ctx context.Context, kind string, id int) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	obj, err := s.mustObject(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	refs, _, err := s.objectRefs(ctx, kind, *obj)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		return nil, &tidyRefusal{Message: fmt.Sprintf("%q is still used; change these first", obj.Name), References: refs}
	}
	docs, err := tidyFetchDocs(ctx, s.client, tidyDocFilter(kind, id))
	if err != nil {
		return nil, err
	}
	if len(docs) > 0 {
		return nil, tidyRefuse("%q still has %d documents; merge it instead", obj.Name, len(docs))
	}
	if kind == "tags" {
		tags, err := tidyFetchObjects(ctx, s.client, "tags")
		if err != nil {
			return nil, err
		}
		if n := len(tidyDescendants(tags, id)); n > 0 {
			return nil, tidyRefuse("%q has %d tags below it, which paperless-ngx would delete with it", obj.Name, n)
		}
	}
	detail := &tidyActionDetail{Object: obj.Raw}
	a := &TidyAction{Action: "delete", Kind: kind, ObjectID: id, ObjectName: obj.Name, Status: "started",
		Summary: fmt.Sprintf("Delete %q (no documents)", obj.Name)}
	s.saveAction(a, detail)
	if err := tidySend(ctx, s.client, "DELETE", fmt.Sprintf("api/%s/%d/", kind, id), nil, nil); err != nil {
		a.Status, a.Error = "failed", err.Error()
		s.saveAction(a, detail)
		return a, err
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// deleteMany deletes several objects that have no documents and nothing
// referring to them, as one logged action that one undo reverses. Objects
// that do not qualify are skipped and named in the summary.
func (s *tidyService) deleteMany(ctx context.Context, kind string, ids []int) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tidyRefuse("nothing selected")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	refs := tidyFetchReferences(ctx, s.client)
	if len(refs.Warnings) > 0 {
		return nil, errors.New(strings.Join(refs.Warnings, "; "))
	}
	cfg := s.configRefs()
	var tags []tidyObject
	if kind == "tags" {
		var err error
		if tags, err = tidyFetchObjects(ctx, s.client, "tags"); err != nil {
			return nil, err
		}
	}
	detail := &tidyActionDetail{}
	var names, skipped []string
	for _, id := range tidyUniqueSorted(ids) {
		obj, err := tidyFetchObject(ctx, s.client, kind, id)
		if err != nil {
			return nil, err
		}
		if obj == nil {
			continue
		}
		why := ""
		if r := tidyRefsFor(append(append([]tidyRef{}, refs.Refs...), tidyConfigReferences(cfg, []tidyObject{*obj})...), kind, *obj); len(r) > 0 {
			why = "in use"
		} else if kind == "tags" && len(tidyDescendants(tags, id)) > 0 {
			why = "has tags below it"
		} else if docs, err := tidyFetchDocs(ctx, s.client, tidyDocFilter(kind, id)); err != nil {
			return nil, err
		} else if len(docs) > 0 {
			why = fmt.Sprintf("%d documents", len(docs))
		}
		if why != "" {
			skipped = append(skipped, fmt.Sprintf("%q (%s)", obj.Name, why))
			continue
		}
		detail.Objects = append(detail.Objects, tidyObjectState{ID: id, Raw: obj.Raw})
		names = append(names, obj.Name)
	}
	if len(detail.Objects) == 0 {
		return nil, tidyRefuse("none of them can be deleted: %s", strings.Join(skipped, ", "))
	}
	a := &TidyAction{Action: "delete_many", Kind: kind, ObjectID: detail.Objects[0].ID, ObjectName: strings.Join(names, ", "), Status: "started",
		Summary: fmt.Sprintf("Delete %d with no documents", len(detail.Objects))}
	if len(skipped) > 0 {
		a.Summary += "; skipped " + strings.Join(skipped, ", ")
	}
	if len(a.ObjectName) > 250 {
		a.ObjectName = a.ObjectName[:250] + "…"
	}
	if len(a.Summary) > 1000 {
		a.Summary = a.Summary[:1000] + "…"
	}
	s.saveAction(a, detail)
	for i, st := range detail.Objects {
		if err := tidySend(ctx, s.client, "DELETE", fmt.Sprintf("api/%s/%d/", kind, st.ID), nil, nil); err != nil {
			a.Status, a.Error = "partial", err.Error()
			if i == 0 {
				a.Status = "failed"
			}
			detail.Objects = detail.Objects[:i]
			s.saveAction(a, detail)
			return a, err
		}
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// --- fixes ---

// setMatching sets the matching algorithm (and, when match is not nil, the
// matching text) on several objects.
func (s *tidyService) setMatching(ctx context.Context, kind string, ids []int, algorithm int, match *string) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	if kind == "custom_fields" {
		return nil, tidyRefuse("custom fields have no matching rules")
	}
	if _, ok := tidyMatchNames[algorithm]; !ok {
		return nil, tidyRefuse("unknown matching algorithm %d", algorithm)
	}
	if len(ids) == 0 {
		return nil, tidyRefuse("nothing selected")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	set := map[string]any{"matching_algorithm": algorithm}
	if match != nil {
		set["match"] = *match
	}
	detail := &tidyActionDetail{Set: set}
	var names []string
	for _, id := range ids {
		obj, err := s.mustObject(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		detail.Objects = append(detail.Objects, tidyObjectState{ID: id, Raw: obj.Raw})
		names = append(names, obj.Name)
	}
	a := &TidyAction{Action: "set_matching", Kind: kind, ObjectID: ids[0], ObjectName: strings.Join(names, ", "), Status: "started",
		Summary: fmt.Sprintf("Set matching to %s on %d", tidyMatchNames[algorithm], len(ids))}
	if len(a.ObjectName) > 250 {
		a.ObjectName = a.ObjectName[:250] + "…"
	}
	s.saveAction(a, detail)
	for i, id := range ids {
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/%s/%d/", kind, id), set, nil); err != nil {
			a.Status, a.Error = "partial", err.Error()
			if i == 0 {
				a.Status = "failed"
			}
			detail.Objects = detail.Objects[:i]
			s.saveAction(a, detail)
			return a, err
		}
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// grantViewer gives the view-only user view permission on objects, keeping
// every other permission and the owner.
func (s *tidyService) grantViewer(ctx context.Context, kind string, ids []int) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	if kind == "custom_fields" {
		return nil, tidyRefuse("custom fields have no permissions")
	}
	if len(ids) == 0 {
		return nil, tidyRefuse("nothing selected")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	viewer, err := tidyFetchUser(ctx, s.client, s.viewer)
	if err != nil {
		return nil, err
	}
	if viewer == nil {
		return nil, tidyRefuse("no paperless-ngx user named %q", s.viewer)
	}
	detail := &tidyActionDetail{}
	var names []string
	for _, id := range ids {
		obj, err := s.mustObject(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		detail.Objects = append(detail.Objects, tidyObjectState{ID: id, Raw: obj.Raw})
		names = append(names, obj.Name)
	}
	a := &TidyAction{Action: "grant_viewer", Kind: kind, ObjectID: ids[0], ObjectName: strings.Join(names, ", "), Status: "started",
		Summary: fmt.Sprintf("Let %s view %d", viewer.Username, len(ids))}
	if len(a.ObjectName) > 250 {
		a.ObjectName = a.ObjectName[:250] + "…"
	}
	s.saveAction(a, detail)
	body := map[string]any{"objects": ids, "object_type": kind, "operation": "set_permissions", "merge": true,
		"permissions": tidyPerms{View: tidyPermSet{Users: []int{viewer.ID}, Groups: []int{}}, Change: tidyPermSet{Users: []int{}, Groups: []int{}}}}
	if err := tidySend(ctx, s.client, "POST", "api/bulk_edit_objects/", body, nil); err != nil {
		a.Status, a.Error = "failed", err.Error()
		s.saveAction(a, detail)
		return a, err
	}
	a.Status = "done"
	s.saveAction(a, detail)
	return a, nil
}

// --- undo ---

func tidySameSet(a, b []int) bool {
	a, b = tidyUniqueSorted(a), tidyUniqueSorted(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// recreate brings back a removed object from its logged JSON. If the object
// still exists (a merge that stopped before deleting it), it is reused.
func (s *tidyService) recreate(ctx context.Context, kind string, raw json.RawMessage) (int, error) {
	var head struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Parent *int   `json:"parent"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return 0, err
	}
	if obj, err := tidyFetchObject(ctx, s.client, kind, head.ID); err != nil {
		return 0, err
	} else if obj != nil && obj.Name == head.Name {
		return obj.ID, nil
	}
	var page struct {
		Results []tidyObject `json:"results"`
	}
	if err := tidySend(ctx, s.client, "GET", fmt.Sprintf("api/%s/?name__iexact=%s", kind, url.QueryEscape(head.Name)), nil, &page); err != nil {
		return 0, err
	}
	if len(page.Results) > 0 {
		return 0, tidyRefuse("an object named %q exists again (id %d); merge or rename it first", head.Name, page.Results[0].ID)
	}
	body, err := tidyCreateBody(kind, raw)
	if err != nil {
		return 0, err
	}
	if head.Parent != nil {
		if p, err := tidyFetchObject(ctx, s.client, "tags", *head.Parent); err != nil {
			return 0, err
		} else if p == nil {
			delete(body, "parent")
		}
	}
	var created tidyObject
	if err := tidySend(ctx, s.client, "POST", fmt.Sprintf("api/%s/", kind), body, &created); err != nil {
		return 0, err
	}
	return created.ID, nil
}

// undo reverses a logged action. force skips documents or objects that
// changed since instead of refusing.
func (s *tidyService) undo(ctx context.Context, id uint, force bool) (*TidyAction, error) {
	if err := s.requireWrites(); err != nil {
		return nil, err
	}
	if s.db == nil {
		return nil, tidyRefuse("no action log")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.invalidate()

	var a TidyAction
	if err := s.db.First(&a, id).Error; err != nil {
		return nil, tidyRefuse("no action %d", id)
	}
	if a.Undone {
		return nil, tidyRefuse("action %d was already undone", id)
	}
	var d tidyActionDetail
	if a.Detail != "" {
		if err := json.Unmarshal([]byte(a.Detail), &d); err != nil {
			return nil, err
		}
	}

	var note string
	var err error
	switch a.Action {
	case "merge":
		note, err = s.undoMerge(ctx, &a, &d, force)
	case "delete_many":
		// An object whose name exists again is left as it is, so a retry
		// after a partial undo picks up where it stopped.
		var made, exists []string
		for _, st := range d.Objects {
			newID, rerr := s.recreate(ctx, a.Kind, st.Raw)
			var refusal *tidyRefusal
			if errors.As(rerr, &refusal) {
				exists = append(exists, refusal.Message)
				continue
			}
			if rerr != nil {
				err = fmt.Errorf("recreated %d, then: %w", len(made), rerr)
				break
			}
			made = append(made, fmt.Sprint(newID))
		}
		if err == nil {
			note = fmt.Sprintf("Recreated %d (new ids %s)", len(made), strings.Join(made, ", "))
			if len(exists) > 0 {
				note += "; not recreated: " + strings.Join(exists, "; ")
			}
		}
	case "delete":
		var newID int
		newID, err = s.recreate(ctx, a.Kind, d.Object)
		if err == nil {
			d.RecreatedID = newID
			note = fmt.Sprintf("Recreated %q as id %d", a.ObjectName, newID)
		}
	case "rename":
		var cur *tidyObject
		cur, err = s.mustObject(ctx, a.Kind, a.ObjectID)
		if err == nil && cur.Name != d.NewName && !force {
			err = tidyRefuse("it is called %q now, not %q", cur.Name, d.NewName)
		}
		if err == nil {
			err = tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/%s/%d/", a.Kind, a.ObjectID), map[string]any{"name": d.OldName}, nil)
			note = fmt.Sprintf("Renamed back to %q", d.OldName)
		}
	case "set_matching":
		note, err = s.undoMatching(ctx, &a, &d, force)
	case "grant_viewer":
		note, err = s.undoPermissions(ctx, &a, &d)
	case "split":
		note, err = s.undoSplit(ctx, &a, &d, force)
	default:
		err = tidyRefuse("cannot undo %q", a.Action)
	}
	if err != nil {
		return nil, err
	}
	now := time.Now()
	a.Undone, a.UndoneAt, a.UndoNote = true, &now, note
	s.saveAction(&a, &d)
	return &a, nil
}

func (s *tidyService) undoMerge(ctx context.Context, a *TidyAction, d *tidyActionDetail, force bool) (string, error) {
	// Which documents still look the way the merge left them?
	var moved []tidyDocChange
	for _, ch := range d.Docs {
		if ch.After != nil {
			moved = append(moved, ch)
		}
	}
	ids := make([]int, 0, len(moved))
	for _, ch := range moved {
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
	var ok []tidyDocChange
	var drifted []int
	for _, ch := range moved {
		doc, exists := byID[ch.ID]
		if !exists || !tidySameSet(doc.refs(a.Kind), ch.After) {
			drifted = append(drifted, ch.ID)
			continue
		}
		ok = append(ok, ch)
	}
	sort.Ints(drifted)
	if len(drifted) > 0 && !force {
		return "", &tidyRefusal{Message: fmt.Sprintf("%d documents changed since the merge; undo the rest anyway?", len(drifted)), DriftedDocs: drifted}
	}

	newID, err := s.recreate(ctx, a.Kind, d.Object)
	if err != nil {
		return "", err
	}
	d.RecreatedID = newID

	for _, child := range d.Children {
		cur, err := tidyFetchObject(ctx, s.client, "tags", child)
		if err != nil {
			return "", err
		}
		if cur != nil && cur.Parent != nil && *cur.Parent == a.TargetID {
			if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/tags/%d/", child), map[string]any{"parent": newID}, nil); err != nil {
				return "", fmt.Errorf("moving child tag %d back: %w", child, err)
			}
		}
	}

	if a.Kind == "tags" {
		// Set each document's tags back exactly. A bulk "remove target"
		// would also strip the target's descendants.
		for _, ch := range ok {
			tags := make([]int, 0, len(ch.Before))
			for _, t := range ch.Before {
				if t == a.ObjectID {
					t = newID
				}
				tags = append(tags, t)
			}
			if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/documents/%d/", ch.ID), map[string]any{"tags": tags}, nil); err != nil {
				return "", fmt.Errorf("restoring tags on document %d: %w", ch.ID, err)
			}
		}
	} else if len(ok) > 0 {
		okIDs := make([]int, 0, len(ok))
		for _, ch := range ok {
			okIDs = append(okIDs, ch.ID)
		}
		if err := tidySetDocRef(ctx, s.client, a.Kind, okIDs, newID); err != nil {
			return "", err
		}
	}

	if d.AliasID != 0 {
		s.db.Delete(&TidyAlias{}, d.AliasID)
	}
	note := fmt.Sprintf("Recreated %q as id %d and moved %d documents back", a.ObjectName, newID, len(ok))
	if len(drifted) > 0 {
		note += fmt.Sprintf("; left %d that changed since: %s", len(drifted), tidyJoinInts(drifted))
	}
	return note, nil
}

func (s *tidyService) undoMatching(ctx context.Context, a *TidyAction, d *tidyActionDetail, force bool) (string, error) {
	var restored, skipped []int
	for _, st := range d.Objects {
		cur, err := tidyFetchObject(ctx, s.client, a.Kind, st.ID)
		if err != nil {
			return "", err
		}
		if cur == nil {
			skipped = append(skipped, st.ID)
			continue
		}
		var prev tidyObject
		if err := json.Unmarshal(st.Raw, &prev); err != nil {
			return "", err
		}
		unchanged := cur.MatchingAlgorithm != nil && float64(*cur.MatchingAlgorithm) == tidyNumber(d.Set["matching_algorithm"])
		if m, ok := d.Set["match"].(string); ok && cur.Match != m {
			unchanged = false
		}
		if !unchanged && !force {
			skipped = append(skipped, st.ID)
			continue
		}
		body := map[string]any{"matching_algorithm": prev.MatchingAlgorithm, "match": prev.Match}
		if err := tidySend(ctx, s.client, "PATCH", fmt.Sprintf("api/%s/%d/", a.Kind, st.ID), body, nil); err != nil {
			return "", err
		}
		restored = append(restored, st.ID)
	}
	note := fmt.Sprintf("Restored matching on %d", len(restored))
	if len(skipped) > 0 {
		note += fmt.Sprintf("; left %d that changed since or are gone: %s", len(skipped), tidyJoinInts(skipped))
	}
	return note, nil
}

func tidyNumber(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return -1
}

func (s *tidyService) undoPermissions(ctx context.Context, a *TidyAction, d *tidyActionDetail) (string, error) {
	restored := 0
	for _, st := range d.Objects {
		var prev tidyObject
		if err := json.Unmarshal(st.Raw, &prev); err != nil {
			return "", err
		}
		perms := prev.Permissions
		if perms == nil {
			perms = &tidyPerms{View: tidyPermSet{Users: []int{}, Groups: []int{}}, Change: tidyPermSet{Users: []int{}, Groups: []int{}}}
		}
		body := map[string]any{"objects": []int{st.ID}, "object_type": a.Kind, "operation": "set_permissions",
			"merge": false, "owner": prev.Owner, "permissions": perms}
		if err := tidySend(ctx, s.client, "POST", "api/bulk_edit_objects/", body, nil); err != nil {
			if apiErr, ok := err.(*tidyAPIError); ok && apiErr.Status == 404 {
				continue
			}
			return "", err
		}
		restored++
	}
	return fmt.Sprintf("Restored permissions on %d", restored), nil
}

// --- aliases inside paperless-gpt ---

// tidyApplyAliases adds Tidy's merged-away names to the name->id maps
// UpdateDocuments resolves LLM suggestions with, so a suggestion of a name
// that was merged away lands on the object it was merged into instead of
// creating it again. A name that matches a real object always wins.
func tidyApplyAliases(db *gorm.DB, correspondents, documentTypes map[string]int) {
	if db == nil || !db.Migrator().HasTable(&TidyAlias{}) {
		return
	}
	var aliases []TidyAlias
	if err := db.Find(&aliases).Error; err != nil {
		log.Debugf("tidy: reading aliases: %v", err)
		return
	}
	for _, al := range aliases {
		var m map[string]int
		switch al.Kind {
		case "correspondents":
			m = correspondents
		case "document_types":
			m = documentTypes
		}
		if m == nil {
			continue
		}
		targetExists := false
		for _, id := range m {
			if id == al.TargetID {
				targetExists = true
				break
			}
		}
		if !targetExists {
			continue
		}
		taken := false
		for name := range m {
			if strings.EqualFold(name, al.Name) {
				taken = true
				break
			}
		}
		if !taken {
			m[al.Name] = al.TargetID
		}
	}
}
