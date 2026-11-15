package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePaperless is a small stateful paperless-ngx: enough of the objects,
// documents, bulk_edit and bulk_edit_objects API for Tidy, with the same
// tag-tree rules as paperless-ngx 3.0 (adding a tag adds its ancestors,
// removing one removes its descendants).
type fakePaperless struct {
	t       *testing.T
	mu      sync.Mutex
	objects map[string]map[int]map[string]any
	docs    map[int]*tidyDoc
	users   []tidyUser
	lists   map[string][]map[string]any // workflows, saved_views, mail_rules
	titles  map[int]string
	nextID  int
	writes  []string
	srv     *httptest.Server
}

func newFakePaperless(t *testing.T) *fakePaperless {
	f := &fakePaperless{t: t, objects: map[string]map[int]map[string]any{}, docs: map[int]*tidyDoc{},
		lists: map[string][]map[string]any{}, titles: map[int]string{}, nextID: 1000}
	for _, k := range tidyKinds {
		f.objects[k] = map[int]map[string]any{}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePaperless) client() *PaperlessClient {
	c := NewPaperlessClient(f.srv.URL, "test-token")
	c.HTTPClient = f.srv.Client()
	return c
}

func (f *fakePaperless) addObject(kind string, id int, name string, extra map[string]any) {
	o := map[string]any{"id": id, "name": name, "match": "", "matching_algorithm": 6, "is_insensitive": true, "owner": nil,
		"permissions": map[string]any{"view": map[string]any{"users": []any{}, "groups": []any{}}, "change": map[string]any{"users": []any{}, "groups": []any{}}}}
	if kind == "tags" {
		o["parent"] = nil
		o["is_inbox_tag"] = false
		o["color"] = "#a6cee3"
	}
	if kind == "custom_fields" {
		o = map[string]any{"id": id, "name": name, "data_type": "string"}
	}
	for k, v := range extra {
		o[k] = v
	}
	f.objects[kind][id] = o
}

func ip(i int) *int { return &i }

func (f *fakePaperless) addDoc(d tidyDoc) {
	if d.Created == "" {
		d.Created = "2024-01-01"
	}
	if d.Added == "" {
		d.Added = "2024-01-02T00:00:00Z"
	}
	if d.Tags == nil {
		d.Tags = []int{}
	}
	f.docs[d.ID] = &d
}

func (f *fakePaperless) parentOf(id int) *int {
	o := f.objects["tags"][id]
	if o == nil {
		return nil
	}
	switch p := o["parent"].(type) {
	case int:
		return &p
	case float64:
		i := int(p)
		return &i
	}
	return nil
}

func (f *fakePaperless) ancestors(id int) []int {
	var out []int
	for p := f.parentOf(id); p != nil; p = f.parentOf(*p) {
		out = append(out, *p)
	}
	return out
}

func (f *fakePaperless) descendants(id int) []int {
	var out []int
	for cid := range f.objects["tags"] {
		for _, a := range f.ancestors(cid) {
			if a == id {
				out = append(out, cid)
			}
		}
	}
	return out
}

func intsAny(ids []int) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = float64(id)
	}
	return out
}

func toInts(v any) []int {
	return tidyInts(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePaperless) docMatches(d *tidyDoc, q map[string][]string) bool {
	for key, vals := range q {
		v := vals[0]
		switch key {
		case "id__in":
			ok := false
			for _, s := range strings.Split(v, ",") {
				if n, _ := strconv.Atoi(s); n == d.ID {
					ok = true
				}
			}
			if !ok {
				return false
			}
		case "tags__id__all":
			n, _ := strconv.Atoi(v)
			if !containsInt(d.Tags, n) {
				return false
			}
		case "custom_fields__id__all":
			n, _ := strconv.Atoi(v)
			if !containsInt(d.refs("custom_fields"), n) {
				return false
			}
		case "correspondent__id__in", "document_type__id__in", "storage_path__id__in":
			n, _ := strconv.Atoi(v)
			kind := map[string]string{"correspondent__id__in": "correspondents", "document_type__id__in": "document_types", "storage_path__id__in": "storage_paths"}[key]
			if !containsInt(d.refs(kind), n) {
				return false
			}
		}
	}
	return true
}

func containsInt(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

func (f *fakePaperless) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(f.t, "Token test-token", r.Header.Get("Authorization"))
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/") // api, kind, [id]
	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			require.NoError(f.t, json.Unmarshal(raw, &body))
		}
	}
	if r.Method != "GET" {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
	}
	q := r.URL.Query()

	switch {
	case path == "api/users":
		var res []tidyUser
		for _, u := range f.users {
			if name := q.Get("username__iexact"); name == "" || strings.EqualFold(name, u.Username) {
				res = append(res, u)
			}
		}
		writeJSON(w, 200, map[string]any{"next": nil, "results": res})
	case path == "api/workflows" || path == "api/saved_views" || path == "api/mail_rules":
		res := f.lists[parts[1]]
		if res == nil {
			res = []map[string]any{}
		}
		writeJSON(w, 200, map[string]any{"next": nil, "results": res})
	case path == "api/documents" && r.Method == "GET":
		var ids []int
		for id, d := range f.docs {
			if f.docMatches(d, q) {
				ids = append(ids, id)
			}
		}
		sort.Ints(ids)
		// Paginate by 2 so callers must follow `next`.
		page, _ := strconv.Atoi(q.Get("page"))
		if page == 0 {
			page = 1
		}
		start, end := (page-1)*2, page*2
		if end > len(ids) {
			end = len(ids)
		}
		var res []*tidyDoc
		if start < len(ids) {
			for _, id := range ids[start:end] {
				res = append(res, f.docs[id])
			}
		}
		var next any
		if end < len(ids) {
			q.Set("page", strconv.Itoa(page+1))
			next = f.srv.URL + r.URL.Path + "?" + q.Encode()
		}
		writeJSON(w, 200, map[string]any{"next": next, "results": res})
	case path == "api/documents/bulk_edit":
		params := body["parameters"].(map[string]any)
		ids := toInts(body["documents"])
		switch body["method"] {
		case "set_correspondent", "set_document_type", "set_storage_path":
			field := strings.TrimPrefix(body["method"].(string), "set_")
			id := int(params[field].(float64))
			for _, did := range ids {
				d := f.docs[did]
				switch field {
				case "correspondent":
					d.Correspondent = ip(id)
				case "document_type":
					d.DocumentType = ip(id)
				case "storage_path":
					d.StoragePath = ip(id)
				}
			}
		case "modify_tags":
			add, remove := toInts(params["add_tags"]), toInts(params["remove_tags"])
			var expRemove []int
			for _, t := range remove {
				expRemove = append(expRemove, t)
				expRemove = append(expRemove, f.descendants(t)...)
			}
			var expAdd []int
			for _, t := range add {
				expAdd = append(expAdd, t)
				expAdd = append(expAdd, f.ancestors(t)...)
			}
			for _, did := range ids {
				d := f.docs[did]
				var kept []int
				for _, t := range d.Tags {
					if !containsInt(expRemove, t) {
						kept = append(kept, t)
					}
				}
				d.Tags = tidyUniqueSorted(append(kept, expAdd...))
			}
		default:
			f.t.Fatalf("unexpected bulk_edit method %v", body["method"])
		}
		writeJSON(w, 200, map[string]any{"result": "OK"})
	case path == "api/bulk_edit_objects":
		kind := body["object_type"].(string)
		require.Equal(f.t, "set_permissions", body["operation"])
		merge, _ := body["merge"].(bool)
		perms := body["permissions"].(map[string]any)
		for _, id := range toInts(body["objects"]) {
			o := f.objects[kind][id]
			if o == nil {
				writeJSON(w, 404, map[string]any{"detail": "not found"})
				return
			}
			if merge {
				if _, ok := o["permissions"].(map[string]any); !ok {
					o["permissions"] = perm()
				}
				cur := o["permissions"].(map[string]any)
				for _, p := range []string{"view", "change"} {
					for _, who := range []string{"users", "groups"} {
						have := toInts(cur[p].(map[string]any)[who])
						add := toInts(perms[p].(map[string]any)[who])
						cur[p].(map[string]any)[who] = intsAny(tidyUniqueSorted(append(have, add...)))
					}
				}
			} else {
				o["permissions"] = perms
				if owner, ok := body["owner"]; ok {
					o["owner"] = owner
				}
			}
		}
		writeJSON(w, 200, map[string]any{"result": "OK"})
	case len(parts) == 3 && parts[1] == "documents" && r.Method == "GET":
		id, _ := strconv.Atoi(parts[2])
		writeJSON(w, 200, map[string]any{"id": id, "title": f.titles[id], "content": "text of " + f.titles[id]})
	case len(parts) == 3 && parts[1] == "documents" && r.Method == "PATCH":
		id, _ := strconv.Atoi(parts[2])
		d := f.docs[id]
		if tags, ok := body["tags"]; ok {
			var full []int
			for _, t := range toInts(tags) {
				full = append(full, t)
				full = append(full, f.ancestors(t)...)
			}
			d.Tags = tidyUniqueSorted(full)
		}
		writeJSON(w, 200, d)
	case len(parts) >= 2 && f.objects[parts[1]] != nil:
		kind := parts[1]
		if len(parts) == 2 {
			switch r.Method {
			case "GET":
				var res []map[string]any
				var ids []int
				for id := range f.objects[kind] {
					ids = append(ids, id)
				}
				sort.Ints(ids)
				for _, id := range ids {
					o := f.objects[kind][id]
					if name := q.Get("name__iexact"); name != "" && !strings.EqualFold(name, o["name"].(string)) {
						continue
					}
					res = append(res, o)
				}
				if res == nil {
					res = []map[string]any{}
				}
				writeJSON(w, 200, map[string]any{"next": nil, "results": res})
			case "POST":
				f.nextID++
				id := f.nextID
				o := map[string]any{"id": id}
				for k, v := range body {
					if k == "set_permissions" {
						o["permissions"] = v
						continue
					}
					o[k] = v
				}
				f.objects[kind][id] = o
				writeJSON(w, 201, o)
			}
			return
		}
		id, _ := strconv.Atoi(parts[2])
		o := f.objects[kind][id]
		if o == nil {
			writeJSON(w, 404, map[string]any{"detail": "No Tag matches the given query."})
			return
		}
		switch r.Method {
		case "GET":
			writeJSON(w, 200, o)
		case "PATCH":
			for k, v := range body {
				o[k] = v
			}
			writeJSON(w, 200, o)
		case "DELETE":
			delete(f.objects[kind], id)
			for _, d := range f.docs {
				switch kind {
				case "tags":
					var kept []int
					for _, t := range d.Tags {
						if t != id {
							kept = append(kept, t)
						}
					}
					d.Tags = kept
				case "correspondents":
					if d.Correspondent != nil && *d.Correspondent == id {
						d.Correspondent = nil
					}
				}
			}
			w.WriteHeader(204)
		}
	default:
		f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
	}
}

func newTestTidy(t *testing.T, f *fakePaperless) *tidyService {
	db, err := InitializeTestDB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&TidyAction{}, &TidyAlias{}, &TidySplitRun{}))
	return &tidyService{client: f.client(), db: db, writes: true, viewer: "mcp-reader",
		configRefs: func() tidyConfigRefs {
			return tidyConfigRefs{TagNames: map[string]string{"paperless-gpt": "AUTO_TAG"}, CustomFieldIDs: map[int]string{}}
		}}
}

func perm(view ...int) map[string]any {
	users := []any{}
	for _, v := range view {
		users = append(users, float64(v))
	}
	return map[string]any{"view": map[string]any{"users": users, "groups": []any{}}, "change": map[string]any{"users": []any{}, "groups": []any{}}}
}

func TestTidyMergeCorrespondentAndUndo(t *testing.T) {
	f := newFakePaperless(t)
	f.users = []tidyUser{{ID: 3, Username: "mcp-reader"}}
	f.addObject("correspondents", 21, "Acme County Credit Union", map[string]any{"matching_algorithm": 3, "match": "Acme County Credit Union"})
	f.addObject("correspondents", 49, "ACCU", map[string]any{"matching_algorithm": 1, "match": "", "owner": 2, "permissions": perm(3)})
	f.addDoc(tidyDoc{ID: 1, Correspondent: ip(49)})
	f.addDoc(tidyDoc{ID: 2, Correspondent: ip(49)})
	f.addDoc(tidyDoc{ID: 3, Correspondent: ip(21)})
	f.addDoc(tidyDoc{ID: 4, Correspondent: ip(49)})
	s := newTestTidy(t, f)
	ctx := context.Background()

	a, err := s.merge(ctx, "correspondents", 49, 21)
	require.NoError(t, err)
	assert.Equal(t, "done", a.Status)
	for _, id := range []int{1, 2, 3, 4} {
		assert.Equal(t, 21, *f.docs[id].Correspondent, "doc %d", id)
	}
	assert.Nil(t, f.objects["correspondents"][49], "the empty source is deleted")

	// paperless-gpt now resolves the old name to the target.
	corr := map[string]int{"Acme County Credit Union": 21}
	tidyApplyAliases(s.db, corr, map[string]int{})
	assert.Equal(t, 21, corr["ACCU"])

	u, err := s.undo(ctx, a.ID, false)
	require.NoError(t, err)
	assert.True(t, u.Undone)
	var recreated map[string]any
	for _, o := range f.objects["correspondents"] {
		if o["name"] == "ACCU" {
			recreated = o
		}
	}
	require.NotNil(t, recreated, "undo recreates the source")
	newID := recreated["id"].(int)
	assert.EqualValues(t, 1, recreated["matching_algorithm"])
	assert.EqualValues(t, 2, recreated["owner"])
	assert.Equal(t, []int{3}, toInts(recreated["permissions"].(map[string]any)["view"].(map[string]any)["users"]), "the view grant comes back")
	for _, id := range []int{1, 2, 4} {
		assert.Equal(t, newID, *f.docs[id].Correspondent, "doc %d moves back", id)
	}
	assert.Equal(t, 21, *f.docs[3].Correspondent, "the target's own document stays")
	corr = map[string]int{"Acme County Credit Union": 21, "ACCU": newID}
	tidyApplyAliases(s.db, corr, map[string]int{})
	assert.Equal(t, newID, corr["ACCU"], "the alias is gone")

	_, err = s.undo(ctx, a.ID, false)
	var refusal *tidyRefusal
	assert.ErrorAs(t, err, &refusal, "an action is undone once")
}

func TestTidyMergeTagMovesChildrenFirstAndUndoRestoresExactTags(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("tags", 3, "Property", nil)
	f.addObject("tags", 4, "Unit 5", map[string]any{"parent": 3})
	f.addObject("tags", 5, "Real Estate", nil)
	f.addObject("tags", 6, "Taxes", nil)
	f.addDoc(tidyDoc{ID: 1, Tags: []int{3, 4}})
	f.addDoc(tidyDoc{ID: 2, Tags: []int{3, 6}})
	f.addDoc(tidyDoc{ID: 3, Tags: []int{3, 5}})
	f.addDoc(tidyDoc{ID: 4, Tags: []int{6}})
	s := newTestTidy(t, f)
	ctx := context.Background()

	a, err := s.merge(ctx, "tags", 3, 5)
	require.NoError(t, err)
	assert.Equal(t, "done", a.Status)
	assert.Equal(t, 5, *f.parentOf(4), "the child moved under the target")
	assert.Equal(t, []int{4, 5}, f.docs[1].Tags, "the child tag survives the merge")
	assert.Equal(t, []int{5, 6}, f.docs[2].Tags)
	assert.Equal(t, []int{5}, f.docs[3].Tags)
	assert.Equal(t, []int{6}, f.docs[4].Tags)
	assert.Nil(t, f.objects["tags"][3])

	_, err = s.undo(ctx, a.ID, false)
	require.NoError(t, err)
	var newID int
	for id, o := range f.objects["tags"] {
		if o["name"] == "Property" {
			newID = id
		}
	}
	require.NotZero(t, newID)
	assert.Equal(t, newID, *f.parentOf(4), "the child moves back")
	assert.ElementsMatch(t, []int{newID, 4}, f.docs[1].Tags)
	assert.ElementsMatch(t, []int{newID, 6}, f.docs[2].Tags, "the target is removed where the merge added it")
	assert.ElementsMatch(t, []int{newID, 5}, f.docs[3].Tags, "the target stays where it was before")
	assert.Equal(t, []int{6}, f.docs[4].Tags)
}

func TestTidyMergeRefusedWhileReferenced(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("tags", 7, "Inbox old", nil)
	f.addObject("tags", 8, "Inbox", nil)
	f.addDoc(tidyDoc{ID: 1, Tags: []int{7}})
	f.lists["workflows"] = []map[string]any{{"id": 1, "name": "Inbox on add", "enabled": true,
		"triggers": []any{map[string]any{"type": 2.0}}, "actions": []any{map[string]any{"type": 1.0, "assign_tags": []any{7.0}}}}}
	s := newTestTidy(t, f)

	_, err := s.merge(context.Background(), "tags", 7, 8)
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal)
	require.Len(t, refusal.References, 1)
	assert.Contains(t, refusal.References[0].Label, `workflow "Inbox on add" action assign_tags`)
	assert.Empty(t, f.writes, "nothing changes")
	assert.Equal(t, []int{7}, f.docs[1].Tags)

	// A tag paperless-gpt names in its config is in use too.
	f.addObject("tags", 9, "paperless-gpt", nil)
	_, err = s.deleteEmpty(context.Background(), "tags", 9)
	require.ErrorAs(t, err, &refusal)
	assert.Contains(t, refusal.References[0].Label, "AUTO_TAG")
	_, err = s.rename(context.Background(), "tags", 9, "paperless-gpt-new")
	require.ErrorAs(t, err, &refusal)
}

func TestTidyUndoRefusesDriftedDocumentsUnlessForced(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("document_types", 1, "Invoice", nil)
	f.addObject("document_types", 2, "Invoice / Receipt", nil)
	f.addObject("document_types", 3, "Letter", nil)
	f.addDoc(tidyDoc{ID: 1, DocumentType: ip(1)})
	f.addDoc(tidyDoc{ID: 2, DocumentType: ip(1)})
	s := newTestTidy(t, f)
	ctx := context.Background()

	a, err := s.merge(ctx, "document_types", 1, 2)
	require.NoError(t, err)
	f.docs[2].DocumentType = ip(3) // changed by hand after the merge

	_, err = s.undo(ctx, a.ID, false)
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, []int{2}, refusal.DriftedDocs)
	assert.Len(t, f.objects["document_types"], 2, "nothing recreated on refusal")

	u, err := s.undo(ctx, a.ID, true)
	require.NoError(t, err)
	assert.Contains(t, u.UndoNote, "left 1 that changed since: 2")
	assert.NotEqual(t, 2, *f.docs[1].DocumentType, "doc 1 moved back")
	assert.Equal(t, 3, *f.docs[2].DocumentType, "doc 2 left alone")
}

func TestTidyMergeStopsBeforeDeletingWhenDocumentsRemain(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("storage_paths", 1, "Financial", nil)
	f.addObject("storage_paths", 2, "Finance", nil)
	f.addDoc(tidyDoc{ID: 1, StoragePath: ip(1)})
	s := newTestTidy(t, f)
	// A document that appears between planning and the re-check is one the
	// merge did not move: the source must survive.
	orig := f.srv.Config.Handler
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/documents/bulk_edit/" {
			orig.ServeHTTP(w, r)
			f.mu.Lock()
			f.addDoc(tidyDoc{ID: 9, StoragePath: ip(1)})
			f.mu.Unlock()
			return
		}
		orig.ServeHTTP(w, r)
	})
	a, err := s.merge(context.Background(), "storage_paths", 1, 2)
	require.Error(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "partial", a.Status)
	assert.NotNil(t, f.objects["storage_paths"][1], "the source is kept")

	// Its undo moves the moved document back onto the surviving source.
	_, err = s.undo(context.Background(), a.ID, false)
	require.NoError(t, err)
	assert.Equal(t, 1, *f.docs[1].StoragePath)
}

func TestTidyDeleteEmptyAndUndo(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("correspondents", 5, "Northwind Office Supplies", map[string]any{"matching_algorithm": 0, "owner": 2})
	f.addObject("correspondents", 6, "Amazon", nil)
	f.addDoc(tidyDoc{ID: 1, Correspondent: ip(6)})
	s := newTestTidy(t, f)
	ctx := context.Background()

	_, err := s.deleteEmpty(ctx, "correspondents", 6)
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal, "an object with documents is not deleted")

	a, err := s.deleteEmpty(ctx, "correspondents", 5)
	require.NoError(t, err)
	assert.Nil(t, f.objects["correspondents"][5])

	_, err = s.undo(ctx, a.ID, false)
	require.NoError(t, err)
	found := false
	for _, o := range f.objects["correspondents"] {
		if o["name"] == "Northwind Office Supplies" {
			found = true
			assert.EqualValues(t, 0, o["matching_algorithm"])
			assert.EqualValues(t, 2, o["owner"])
		}
	}
	assert.True(t, found)
}

func TestTidyDeleteRefusesTagWithChildren(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("tags", 1, "Real Estate", nil)
	f.addObject("tags", 2, "Villa", map[string]any{"parent": 1})
	s := newTestTidy(t, f)
	_, err := s.deleteEmpty(context.Background(), "tags", 1)
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal)
	assert.NotNil(t, f.objects["tags"][2])
}

func TestTidyRenameAndUndo(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("correspondents", 1, "AcmeHardware", nil)
	s := newTestTidy(t, f)
	a, err := s.rename(context.Background(), "correspondents", 1, "Acme Hardware")
	require.NoError(t, err)
	assert.Equal(t, "Acme Hardware", f.objects["correspondents"][1]["name"])
	_, err = s.undo(context.Background(), a.ID, false)
	require.NoError(t, err)
	assert.Equal(t, "AcmeHardware", f.objects["correspondents"][1]["name"])
}

func TestTidySetMatchingAndUndo(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("tags", 1, "Income", map[string]any{"matching_algorithm": 1, "match": ""})
	f.addObject("tags", 2, "Payment", map[string]any{"matching_algorithm": 1, "match": ""})
	s := newTestTidy(t, f)
	a, err := s.setMatching(context.Background(), "tags", []int{1, 2}, tidyMatchNone, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 0, f.objects["tags"][1]["matching_algorithm"])
	f.objects["tags"][2]["matching_algorithm"] = 6 // changed since
	u, err := s.undo(context.Background(), a.ID, false)
	require.NoError(t, err)
	assert.EqualValues(t, 1, f.objects["tags"][1]["matching_algorithm"])
	assert.EqualValues(t, 6, f.objects["tags"][2]["matching_algorithm"], "a changed object is left alone")
	assert.Contains(t, u.UndoNote, "left 1")
}

func TestTidyGrantViewerAndUndo(t *testing.T) {
	f := newFakePaperless(t)
	f.users = []tidyUser{{ID: 3, Username: "mcp-reader"}}
	f.addObject("correspondents", 1, "Example Bank", map[string]any{"owner": 2, "permissions": perm(7)})
	s := newTestTidy(t, f)
	a, err := s.grantViewer(context.Background(), "correspondents", []int{1})
	require.NoError(t, err)
	view := f.objects["correspondents"][1]["permissions"].(map[string]any)["view"].(map[string]any)
	assert.Equal(t, []int{3, 7}, toInts(view["users"]), "the grant is added to existing ones")
	_, err = s.undo(context.Background(), a.ID, false)
	require.NoError(t, err)
	view = f.objects["correspondents"][1]["permissions"].(map[string]any)["view"].(map[string]any)
	assert.Equal(t, []int{7}, toInts(view["users"]))
	assert.EqualValues(t, 2, f.objects["correspondents"][1]["owner"])
}

func TestTidyReadOnlyRefusesWrites(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("correspondents", 1, "A", nil)
	f.addObject("correspondents", 2, "B", nil)
	s := newTestTidy(t, f)
	s.writes = false
	_, err := s.merge(context.Background(), "correspondents", 1, 2)
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Contains(t, refusal.Message, "TIDY_WRITES")
	assert.Empty(t, f.writes)
}

func TestTidyInventoryFindings(t *testing.T) {
	f := newFakePaperless(t)
	f.users = []tidyUser{{ID: 3, Username: "mcp-reader"}}
	f.addObject("correspondents", 21, "Acme County Credit Union", map[string]any{"matching_algorithm": 3, "match": "Acme County Credit Union"})
	f.addObject("correspondents", 49, "ACCU", map[string]any{"matching_algorithm": 1, "match": ""})
	f.addObject("correspondents", 26, "B&Q", nil)
	f.addObject("correspondents", 27, "B & Q Ltd.", nil)
	f.addObject("correspondents", 33, "ABC", nil)
	f.addObject("correspondents", 34, "WXYZ", nil)
	f.addObject("correspondents", 24, "Federal Agency", map[string]any{"match": "Department of"})
	f.addObject("correspondents", 418, "Northwind Office Supplies", map[string]any{"owner": 2, "permissions": perm()})
	f.addObject("correspondents", 420, "Example Bank", map[string]any{"owner": 2, "permissions": perm(3)})
	f.addObject("tags", 1, "Real Estate", nil)
	f.addObject("tags", 2, "Inbox", map[string]any{"is_inbox_tag": true}) // empty by design
	f.addObject("custom_fields", 24, "Tax Year", nil)
	f.addDoc(tidyDoc{ID: 1, Correspondent: ip(21), Created: "2025-03-01", Added: "2025-03-02T00:00:00Z", Tags: []int{1}})
	f.addDoc(tidyDoc{ID: 2, Correspondent: ip(21), Created: "2026-01-05", Added: "2025-12-31T00:00:00Z"})
	f.lists["saved_views"] = []map[string]any{{"id": 4, "name": "Old bank", "sort_field": "created", "display_fields": []any{"custom_field_24"},
		"filter_rules": []any{map[string]any{"rule_type": 3.0, "value": "999"}}}}
	f.lists["workflows"] = []map[string]any{{"id": 1, "name": "AI", "enabled": true,
		"triggers": []any{map[string]any{"type": 3.0, "filter_has_tags": []any{1.0}}}, "actions": []any{}}}
	s := newTestTidy(t, f)

	inv, err := s.inventory(context.Background(), true)
	require.NoError(t, err)

	byType := map[string][]tidyFinding{}
	for _, fd := range inv.Findings {
		byType[fd.Kind+"/"+fd.Type] = append(byType[fd.Kind+"/"+fd.Type], fd)
	}
	var groups [][]int
	for _, fd := range byType["correspondents/near_duplicate"] {
		groups = append(groups, fd.IDs)
	}
	assert.ElementsMatch(t, [][]int{{21, 49}, {26, 27}}, groups, "ACCU and B&Q pair up; ABC and WXYZ do not")
	assert.Equal(t, []int{49}, byType["correspondents/dead_rule"][0].IDs)
	assert.Equal(t, []int{24}, byType["correspondents/ignored_rule_text"][0].IDs)
	assert.Equal(t, []int{418}, byType["correspondents/hidden_from_viewer"][0].IDs)
	assert.Contains(t, byType["correspondents/unused"][0].IDs, 418)
	assert.NotContains(t, byType["correspondents/unused"][0].IDs, 21)
	require.Len(t, byType["correspondents/stale_reference"], 1)
	assert.Contains(t, byType["correspondents/stale_reference"][0].Detail, `saved view "Old bank" filter refers to id 999`)
	require.Len(t, byType["tags/stale_reference"], 1, "AUTO_TAG names a tag that does not exist")
	assert.Contains(t, byType["tags/stale_reference"][0].Detail, `"paperless-gpt"`)

	var accu, union tidyRow
	for _, r := range inv.Objects["correspondents"] {
		switch r.ID {
		case 21:
			union = r
		case 49:
			accu = r
		}
	}
	assert.Equal(t, 2, union.DocCount)
	assert.Equal(t, "2026-01-05", union.LastCreated)
	assert.Equal(t, "2025-12-31T00:00:00Z", union.LastAdded)
	assert.Equal(t, 0, accu.DocCount)
	assert.Equal(t, []tidyWorkflowInfo{{ID: 1, Name: "AI", Enabled: true}}, inv.UpdatedWorkflows)
	assert.Empty(t, byType["tags/unused"], "an empty tag that is in use (the inbox) is not listed")
	var realEstate tidyRow
	for _, r := range inv.Objects["tags"] {
		if r.ID == 1 {
			realEstate = r
		}
	}
	require.Len(t, realEstate.References, 1)
	assert.Equal(t, "workflow", realEstate.References[0].Source)
	assert.Len(t, inv.Objects["custom_fields"][0].References, 1, "the saved view's column counts as a use")
}

func TestTidyNearDuplicateRules(t *testing.T) {
	objs := func(names ...string) []tidyObject {
		var out []tidyObject
		for i, n := range names {
			out = append(out, tidyObject{ID: i + 1, Name: n})
		}
		return out
	}
	cases := []struct {
		names []string
		want  int // number of groups
	}{
		{[]string{"AcmeHardware", "Acme Hardware"}, 1},
		{[]string{"Globex", "Globex Inc."}, 1},
		{[]string{"Springfeild County", "Springfield County"}, 1},
		{[]string{"State Government", "Local Government"}, 0},
		{[]string{"John Q Smith", "Jane R Smith"}, 0},
		{[]string{"US Bank", "WXYZ"}, 0},
		{[]string{"Income", "Income Tax"}, 0},
	}
	for _, c := range cases {
		assert.Len(t, tidyNearDuplicates(objs(c.names...)), c.want, "%v", c.names)
	}
}

func TestTidyParseLLMGroups(t *testing.T) {
	known := map[int]bool{21: true, 49: true, 3: true}
	answer := "<think>hmm</think>Here you go:\n```json\n[{\"ids\":[49,21],\"keep\":21,\"reason\":\"abbreviation\"},{\"ids\":[3,99],\"keep\":3},{\"ids\":[3,21],\"keep\":7}]\n```"
	groups, err := tidyParseLLMGroups(answer, known)
	require.NoError(t, err)
	assert.Equal(t, []tidyLLMGroup{{IDs: []int{21, 49}, Keep: 21, Reason: "abbreviation"}}, groups,
		"unknown ids and a keep outside the group are dropped")
	_, err = tidyParseLLMGroups("no idea", known)
	assert.Error(t, err)
}

func TestTidyApplyAliasesRealNameWins(t *testing.T) {
	db, err := InitializeTestDB()
	require.NoError(t, err)
	tidyApplyAliases(db, map[string]int{}, map[string]int{}) // no table yet: no-op
	require.NoError(t, db.AutoMigrate(&TidyAlias{}))
	require.NoError(t, db.Create(&TidyAlias{Kind: "correspondents", Name: "ACCU", TargetID: 21}).Error)
	require.NoError(t, db.Create(&TidyAlias{Kind: "correspondents", Name: "Gone", TargetID: 77}).Error)
	require.NoError(t, db.Create(&TidyAlias{Kind: "document_types", Name: "Receipt", TargetID: 9}).Error)

	corr := map[string]int{"Acme County Credit Union": 21, "accu": 50}
	types := map[string]int{"Invoice / Receipt": 9}
	tidyApplyAliases(db, corr, types)
	assert.Equal(t, map[string]int{"Acme County Credit Union": 21, "accu": 50}, corr,
		"a real object with the name wins, and an alias whose target is gone is skipped")
	assert.Equal(t, 9, types["Receipt"])
	_, _, ok := lookupNamedID("Document type", types, "receipt")
	assert.True(t, ok)
}

func TestTidyCreateBodyKeepsRulesAndPermissions(t *testing.T) {
	raw := json.RawMessage(`{"id":5,"slug":"x","name":"X","match":"a b","matching_algorithm":1,"is_insensitive":false,"owner":2,
		"document_count":3,"user_can_change":true,"permissions":{"view":{"users":[3],"groups":[]},"change":{"users":[],"groups":[]}},
		"parent":1,"children":[],"color":"#fff","is_inbox_tag":false}`)
	body, err := tidyCreateBody("tags", raw)
	require.NoError(t, err)
	for _, k := range []string{"id", "slug", "document_count", "user_can_change", "children", "permissions"} {
		assert.NotContains(t, body, k)
	}
	assert.Equal(t, "a b", body["match"])
	assert.EqualValues(t, 1, body["parent"])
	assert.NotNil(t, body["set_permissions"])
}
