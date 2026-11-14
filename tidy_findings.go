package main

// Tidy: the inventory (one row per object, with document counts and last use),
// the places that refer to objects (workflows, saved views, mail rules,
// paperless-gpt's own settings), and the findings computed from both.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Matching algorithms, as paperless-ngx numbers them.
const (
	tidyMatchNone    = 0
	tidyMatchAny     = 1
	tidyMatchAll     = 2
	tidyMatchLiteral = 3
	tidyMatchRegex   = 4
	tidyMatchFuzzy   = 5
	tidyMatchAuto    = 6
)

var tidyMatchNames = map[int]string{
	tidyMatchNone: "None", tidyMatchAny: "Any word", tidyMatchAll: "All words", tidyMatchLiteral: "Exact match",
	tidyMatchRegex: "Regular expression", tidyMatchFuzzy: "Fuzzy word", tidyMatchAuto: "Automatic",
}

// tidyRef is one place that points at an object.
type tidyRef struct {
	Kind     string `json:"kind"`      // object type referred to
	ObjectID int    `json:"object_id"` // 0 when referred to by a name that matches nothing
	Name     string `json:"name,omitempty"`
	Source   string `json:"source"` // workflow, saved_view, mail_rule, paperless-gpt, inbox
	SourceID string `json:"source_id,omitempty"`
	Label    string `json:"label"` // human description of where
	ByName   bool   `json:"by_name,omitempty"`
}

// tidyWorkflowInfo names a paperless-ngx workflow that runs on Document Updated.
type tidyWorkflowInfo struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type tidyRow struct {
	ID                int       `json:"id"`
	Name              string    `json:"name"`
	DocCount          int       `json:"doc_count"`
	LastCreated       string    `json:"last_created,omitempty"`
	LastAdded         string    `json:"last_added,omitempty"`
	MatchingAlgorithm *int      `json:"matching_algorithm,omitempty"`
	Match             string    `json:"match"`
	Owner             *int      `json:"owner"`
	OwnerName         string    `json:"owner_name,omitempty"`
	Parent            *int      `json:"parent,omitempty"`
	IsInboxTag        bool      `json:"is_inbox_tag,omitempty"`
	DataType          string    `json:"data_type,omitempty"`
	VisibleToViewer   *bool     `json:"visible_to_viewer,omitempty"`
	References        []tidyRef `json:"references"`
}

type tidyFinding struct {
	Kind   string `json:"kind"`
	Type   string `json:"type"` // unused, near_duplicate, dead_rule, ignored_rule_text, hidden_from_viewer, stale_reference
	IDs    []int  `json:"ids"`
	Detail string `json:"detail"`
}

type tidyInventory struct {
	GeneratedAt      time.Time                 `json:"generated_at"`
	Writes           bool                      `json:"writes"`
	Viewer           string                    `json:"viewer"`
	ViewerID         int                       `json:"viewer_id"`
	ViewerFound      bool                      `json:"viewer_found"`
	DocumentCount    int                       `json:"document_count"`
	Objects          map[string][]tidyRow      `json:"objects"`
	Findings         []tidyFinding             `json:"findings"`
	MatchingSummary  map[string]map[string]int `json:"matching_summary"`
	UpdatedWorkflows []tidyWorkflowInfo        `json:"updated_workflows"`
	Aliases          []TidyAlias               `json:"aliases"`
	Warnings         []string                  `json:"warnings"`
}

// tidyConfigRefs is what paperless-gpt itself refers to: tags by name (its
// environment and its own workflows) and custom fields by id (settings).
type tidyConfigRefs struct {
	TagNames       map[string]string // tag name -> where ("AUTO_TAG")
	CustomFieldIDs map[int]string    // field id -> where
}

// tidyCurrentConfigRefs reads paperless-gpt's live configuration.
func tidyCurrentConfigRefs() tidyConfigRefs {
	refs := tidyConfigRefs{TagNames: map[string]string{}, CustomFieldIDs: map[int]string{}}
	add := func(name, where string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if prev, ok := refs.TagNames[name]; ok {
			refs.TagNames[name] = prev + ", " + where
		} else {
			refs.TagNames[name] = where
		}
	}
	add(manualTag, "MANUAL_TAG")
	add(autoTag, "AUTO_TAG")
	add(autoTagComplete, "AUTO_TAG_COMPLETE")
	add(autoOcrTag, "AUTO_OCR_TAG")
	add(failTag, "FAIL_TAG")
	if pdfOCRTagging {
		add(pdfOCRCompleteTag, "PDF_OCR_COMPLETE_TAG")
	}
	for _, wf := range workflows.List() {
		add(wf.TriggerTag, fmt.Sprintf("paperless-gpt workflow %q trigger", wf.Name))
		add(wf.CompletionTag, fmt.Sprintf("paperless-gpt workflow %q completion", wf.Name))
		for _, id := range wf.CustomFieldIDs {
			refs.CustomFieldIDs[id] = fmt.Sprintf("paperless-gpt workflow %q custom fields", wf.Name)
		}
	}
	settingsMutex.RLock()
	for _, id := range settings.CustomFieldsSelectedIDs {
		refs.CustomFieldIDs[id] = "paperless-gpt settings: custom fields to fill"
	}
	settingsMutex.RUnlock()
	return refs
}

// tidyKindForKey maps a workflow or mail rule field name to the object kind
// it refers to, or "" when it refers to none.
func tidyKindForKey(key string) string {
	k := strings.ToLower(key)
	if strings.HasSuffix(k, "_from") || strings.HasSuffix(k, "_from_rule") || k == "assign_custom_fields_values" || k == "filter_custom_field_query" {
		return ""
	}
	switch {
	case strings.Contains(k, "custom_field"):
		return "custom_fields"
	case strings.Contains(k, "storage_path"):
		return "storage_paths"
	case strings.Contains(k, "document_type"):
		return "document_types"
	case strings.Contains(k, "correspondent"):
		return "correspondents"
	case strings.Contains(k, "tag"):
		return "tags"
	}
	return ""
}

// tidyInts reads an id or a list of ids from decoded JSON.
func tidyInts(v any) []int {
	switch x := v.(type) {
	case float64:
		return []int{int(x)}
	case []any:
		var out []int
		for _, e := range x {
			if f, ok := e.(float64); ok {
				out = append(out, int(f))
			}
		}
		return out
	}
	return nil
}

// tidyCustomFieldQueryIDs finds the field ids in a paperless-ngx custom field
// query such as ["AND", [[24, "exists", true], [10, "gt", 5]]].
func tidyCustomFieldQueryIDs(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	if len(arr) == 3 {
		if f, ok := arr[0].(float64); ok {
			if _, ok := arr[1].(string); ok {
				return []int{int(f)}
			}
		}
	}
	var out []int
	for _, e := range arr {
		out = append(out, tidyCustomFieldQueryIDs(e)...)
	}
	return out
}

func tidyParseQuery(s string) []int {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	return tidyCustomFieldQueryIDs(v)
}

// tidyScanEntry collects references from one workflow trigger or action, or
// one mail rule: every top-level field whose name names an object kind.
func tidyScanEntry(entry map[string]any, source, sourceID, label string) []tidyRef {
	var refs []tidyRef
	keys := make([]string, 0, len(entry))
	for k := range entry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		val := entry[key]
		switch key {
		case "assign_custom_fields_values":
			if m, ok := val.(map[string]any); ok {
				for k := range m {
					if id, err := strconv.Atoi(k); err == nil {
						refs = append(refs, tidyRef{Kind: "custom_fields", ObjectID: id, Source: source, SourceID: sourceID, Label: label + " " + key})
					}
				}
			}
			continue
		case "filter_custom_field_query":
			if s, ok := val.(string); ok && s != "" {
				for _, id := range tidyParseQuery(s) {
					refs = append(refs, tidyRef{Kind: "custom_fields", ObjectID: id, Source: source, SourceID: sourceID, Label: label + " " + key})
				}
			}
			continue
		}
		kind := tidyKindForKey(key)
		if kind == "" {
			continue
		}
		for _, id := range tidyInts(val) {
			refs = append(refs, tidyRef{Kind: kind, ObjectID: id, Source: source, SourceID: sourceID, Label: label + " " + key})
		}
	}
	return refs
}

// Saved view filter rule types that hold an object id (paperless-ngx
// SavedViewFilterRule.RULE_TYPES).
var tidyRuleKinds = map[int]string{
	3: "correspondents", 26: "correspondents", 27: "correspondents",
	4: "document_types", 28: "document_types", 29: "document_types",
	6: "tags", 17: "tags", 22: "tags",
	25: "storage_paths", 30: "storage_paths", 31: "storage_paths",
	39: "custom_fields", 40: "custom_fields",
}

// tidyReferences reads workflows, saved views and mail rules. Updated lists the
// workflows triggered by Document Updated, which bulk edits set off.
type tidyReferences struct {
	Refs     []tidyRef
	Updated  []tidyWorkflowInfo
	Warnings []string
}

func tidyFetchReferences(ctx context.Context, c tidyHTTP) tidyReferences {
	var out tidyReferences

	wfs, err := tidyListAll(ctx, c, "api/workflows/?page_size=1000")
	if err != nil {
		out.Warnings = append(out.Warnings, "Could not read paperless-ngx workflows: "+err.Error())
	}
	for _, raw := range wfs {
		var wf struct {
			ID       int              `json:"id"`
			Name     string           `json:"name"`
			Enabled  bool             `json:"enabled"`
			Triggers []map[string]any `json:"triggers"`
			Actions  []map[string]any `json:"actions"`
		}
		if json.Unmarshal(raw, &wf) != nil {
			continue
		}
		sid := strconv.Itoa(wf.ID)
		label := fmt.Sprintf("workflow %q", wf.Name)
		updated := false
		for _, t := range wf.Triggers {
			if typ, ok := t["type"].(float64); ok && int(typ) == 3 {
				updated = true
			}
			out.Refs = append(out.Refs, tidyScanEntry(t, "workflow", sid, label+" trigger")...)
		}
		for _, a := range wf.Actions {
			out.Refs = append(out.Refs, tidyScanEntry(a, "workflow", sid, label+" action")...)
		}
		if updated {
			out.Updated = append(out.Updated, tidyWorkflowInfo{ID: wf.ID, Name: wf.Name, Enabled: wf.Enabled})
		}
	}

	views, err := tidyListAll(ctx, c, "api/saved_views/?page_size=1000")
	if err != nil {
		out.Warnings = append(out.Warnings, "Could not read saved views: "+err.Error())
	}
	for _, raw := range views {
		var v struct {
			ID            int      `json:"id"`
			Name          string   `json:"name"`
			SortField     string   `json:"sort_field"`
			DisplayFields []string `json:"display_fields"`
			FilterRules   []struct {
				RuleType int     `json:"rule_type"`
				Value    *string `json:"value"`
			} `json:"filter_rules"`
		}
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		sid := strconv.Itoa(v.ID)
		label := fmt.Sprintf("saved view %q", v.Name)
		for _, r := range v.FilterRules {
			if r.Value == nil {
				continue
			}
			if r.RuleType == 42 {
				for _, id := range tidyParseQuery(*r.Value) {
					out.Refs = append(out.Refs, tidyRef{Kind: "custom_fields", ObjectID: id, Source: "saved_view", SourceID: sid, Label: label + " custom field query"})
				}
				continue
			}
			kind, ok := tidyRuleKinds[r.RuleType]
			if !ok {
				continue
			}
			for _, part := range strings.Split(*r.Value, ",") {
				if id, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
					out.Refs = append(out.Refs, tidyRef{Kind: kind, ObjectID: id, Source: "saved_view", SourceID: sid, Label: label + " filter"})
				}
			}
		}
		fields := append([]string{v.SortField}, v.DisplayFields...)
		for _, f := range fields {
			if rest, ok := strings.CutPrefix(f, "custom_field_"); ok {
				if id, err := strconv.Atoi(rest); err == nil {
					out.Refs = append(out.Refs, tidyRef{Kind: "custom_fields", ObjectID: id, Source: "saved_view", SourceID: sid, Label: label + " column or sort"})
				}
			}
		}
	}

	rules, err := tidyListAll(ctx, c, "api/mail_rules/?page_size=1000")
	if err != nil {
		out.Warnings = append(out.Warnings, "Could not read mail rules: "+err.Error())
	}
	for _, raw := range rules {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		id, _ := m["id"].(float64)
		name, _ := m["name"].(string)
		out.Refs = append(out.Refs, tidyScanEntry(m, "mail_rule", strconv.Itoa(int(id)), fmt.Sprintf("mail rule %q", name))...)
	}
	return out
}

// tidyConfigReferences resolves paperless-gpt's own references against the
// current tags. A tag name that matches no tag becomes a reference to id 0.
func tidyConfigReferences(cfg tidyConfigRefs, tags []tidyObject) []tidyRef {
	var refs []tidyRef
	names := make([]string, 0, len(cfg.TagNames))
	for n := range cfg.TagNames {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		id := 0
		for _, t := range tags {
			if strings.EqualFold(t.Name, name) {
				id = t.ID
				break
			}
		}
		refs = append(refs, tidyRef{Kind: "tags", ObjectID: id, Name: name, Source: "paperless-gpt", Label: "paperless-gpt " + cfg.TagNames[name], ByName: true})
	}
	ids := make([]int, 0, len(cfg.CustomFieldIDs))
	for id := range cfg.CustomFieldIDs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		refs = append(refs, tidyRef{Kind: "custom_fields", ObjectID: id, Source: "paperless-gpt", Label: cfg.CustomFieldIDs[id]})
	}
	return refs
}

// tidyRefsFor returns the references to one object, including its being the
// inbox tag.
func tidyRefsFor(all []tidyRef, kind string, obj tidyObject) []tidyRef {
	out := []tidyRef{}
	for _, r := range all {
		if r.Kind == kind && r.ObjectID == obj.ID && obj.ID != 0 {
			out = append(out, r)
		}
	}
	if kind == "tags" && obj.IsInboxTag {
		out = append(out, tidyRef{Kind: kind, ObjectID: obj.ID, Source: "inbox", Label: "paperless-ngx inbox tag"})
	}
	return out
}

// tidyVisibleTo reports whether user can see obj in paperless-ngx: no owner,
// owned by the user, or granted view or change to the user or one of its groups.
func tidyVisibleTo(obj tidyObject, user *tidyUser) bool {
	if user == nil || user.IsSuperuser || obj.Owner == nil || *obj.Owner == user.ID {
		return true
	}
	if obj.Permissions == nil {
		return false
	}
	has := func(list []int, id int) bool {
		for _, x := range list {
			if x == id {
				return true
			}
		}
		return false
	}
	if has(obj.Permissions.View.Users, user.ID) || has(obj.Permissions.Change.Users, user.ID) {
		return true
	}
	for _, g := range user.Groups {
		if has(obj.Permissions.View.Groups, g) || has(obj.Permissions.Change.Groups, g) {
			return true
		}
	}
	return false
}

// --- near-duplicate names ---

var tidyNameStopwords = map[string]bool{
	"the": true, "inc": true, "llc": true, "ltd": true, "co": true, "corp": true, "corporation": true,
	"company": true, "and": true, "of": true, "plc": true, "gmbh": true, "sa": true, "sarl": true, "sal": true,
}

// tidyNameWords lowercases, turns & into "and" and splits on
// anything that is not a letter or digit.
func tidyNameWords(name string) []string {
	s := strings.ReplaceAll(strings.ToLower(name), "&", " and ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

// tidyNameKey is a name with case, punctuation, spaces and company
// suffixes removed: "B&Q" and "B & Q Ltd." share one key.
func tidyNameKey(name string) string {
	var b strings.Builder
	for _, w := range tidyNameWords(name) {
		if !tidyNameStopwords[w] {
			b.WriteString(w)
		}
	}
	if b.Len() == 0 {
		return strings.Join(tidyNameWords(name), "")
	}
	return b.String()
}

// tidyInitials are the first letters of a name's words, without stopwords.
func tidyInitials(name string) string {
	var b strings.Builder
	words := 0
	for _, w := range tidyNameWords(name) {
		if tidyNameStopwords[w] {
			continue
		}
		b.WriteRune([]rune(w)[0])
		words++
	}
	if words < 2 {
		return ""
	}
	return b.String()
}

// tidyAcronym returns a single-word name of 2 to 8 letters, lowercased, as a
// candidate acronym; "" for anything else.
func tidyAcronym(name string) string {
	words := tidyNameWords(name)
	if len(words) != 1 {
		return ""
	}
	n := len([]rune(words[0]))
	if n < 2 || n > 8 {
		return ""
	}
	return words[0]
}

func tidyEditDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// tidyNearDuplicates groups objects whose names look like the same thing:
// equal keys, an acronym of another's initials, or keys one or two edits apart
// (for keys of 6+ characters). Each group is sorted by id.
func tidyNearDuplicates(objs []tidyObject) [][]int {
	parent := map[int]int{}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) { parent[find(a)] = find(b) }
	for _, o := range objs {
		parent[o.ID] = o.ID
	}
	keys := make([]string, len(objs))
	for i, o := range objs {
		keys[i] = tidyNameKey(o.Name)
	}
	for i := range objs {
		for j := i + 1; j < len(objs); j++ {
			a, b := objs[i], objs[j]
			ka, kb := keys[i], keys[j]
			same := ka != "" && ka == kb
			if !same {
				if ac := tidyAcronym(a.Name); ac != "" && ac == tidyInitials(b.Name) {
					same = true
				} else if ac := tidyAcronym(b.Name); ac != "" && ac == tidyInitials(a.Name) {
					same = true
				}
			}
			if !same && len(ka) >= 6 && len(kb) >= 6 {
				d := tidyEditDistance(ka, kb)
				same = d <= 1 || (d == 2 && len(ka) >= 10 && len(kb) >= 10)
			}
			if same {
				union(a.ID, b.ID)
			}
		}
	}
	groups := map[int][]int{}
	for _, o := range objs {
		r := find(o.ID)
		groups[r] = append(groups[r], o.ID)
	}
	var out [][]int
	for _, g := range groups {
		if len(g) > 1 {
			sort.Ints(g)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// --- assembling the inventory ---

// tidyBuildInventory turns fetched objects, documents and references into rows
// and findings. It does no I/O so it can be tested directly.
func tidyBuildInventory(objects map[string][]tidyObject, docs []tidyDoc, refs []tidyRef, viewer *tidyUser, users map[int]string) (map[string][]tidyRow, []tidyFinding, map[string]map[string]int) {
	type usage struct {
		count          int
		created, added string
	}
	rows := map[string][]tidyRow{}
	var findings []tidyFinding
	summary := map[string]map[string]int{}

	for _, kind := range tidyKinds {
		use := map[int]*usage{}
		for _, d := range docs {
			for _, id := range tidyUniqueSorted(d.refs(kind)) {
				u := use[id]
				if u == nil {
					u = &usage{}
					use[id] = u
				}
				u.count++
				if d.Created > u.created {
					u.created = d.Created
				}
				if d.Added > u.added {
					u.added = d.Added
				}
			}
		}

		objs := objects[kind]
		known := map[int]bool{}
		kindRows := make([]tidyRow, 0, len(objs))
		var unused, dead, ignored, hidden []int
		for _, o := range objs {
			known[o.ID] = true
			row := tidyRow{ID: o.ID, Name: o.Name, MatchingAlgorithm: o.MatchingAlgorithm, Match: o.Match, Owner: o.Owner,
				Parent: o.Parent, IsInboxTag: o.IsInboxTag, DataType: o.DataType, References: tidyRefsFor(refs, kind, o)}
			if u := use[o.ID]; u != nil {
				row.DocCount, row.LastCreated, row.LastAdded = u.count, u.created, u.added
			}
			if o.Owner != nil {
				row.OwnerName = users[*o.Owner]
			}
			if kind != "custom_fields" {
				vis := tidyVisibleTo(o, viewer)
				row.VisibleToViewer = &vis
				if !vis {
					hidden = append(hidden, o.ID)
				}
			}
			if row.DocCount == 0 {
				unused = append(unused, o.ID)
			}
			if o.MatchingAlgorithm != nil {
				algo := *o.MatchingAlgorithm
				if summary[kind] == nil {
					summary[kind] = map[string]int{}
				}
				summary[kind][tidyMatchNames[algo]]++
				text := strings.TrimSpace(o.Match)
				if text == "" && algo >= tidyMatchAny && algo <= tidyMatchFuzzy {
					dead = append(dead, o.ID)
				}
				if text != "" && (algo == tidyMatchNone || algo == tidyMatchAuto) {
					ignored = append(ignored, o.ID)
				}
			}
			kindRows = append(kindRows, row)
		}
		sort.Slice(kindRows, func(i, j int) bool { return strings.ToLower(kindRows[i].Name) < strings.ToLower(kindRows[j].Name) })
		rows[kind] = kindRows

		if len(unused) > 0 {
			findings = append(findings, tidyFinding{Kind: kind, Type: "unused", IDs: unused,
				Detail: fmt.Sprintf("%d with no documents", len(unused))})
		}
		for _, g := range tidyNearDuplicates(objs) {
			findings = append(findings, tidyFinding{Kind: kind, Type: "near_duplicate", IDs: g, Detail: "Names that look like the same thing"})
		}
		if len(dead) > 0 {
			findings = append(findings, tidyFinding{Kind: kind, Type: "dead_rule", IDs: dead,
				Detail: fmt.Sprintf("%d with a matching rule that needs text but has none, so it never matches", len(dead))})
		}
		if len(ignored) > 0 {
			findings = append(findings, tidyFinding{Kind: kind, Type: "ignored_rule_text", IDs: ignored,
				Detail: fmt.Sprintf("%d with matching text that their algorithm (None or Automatic) ignores", len(ignored))})
		}
		if len(hidden) > 0 && viewer != nil {
			findings = append(findings, tidyFinding{Kind: kind, Type: "hidden_from_viewer", IDs: hidden,
				Detail: fmt.Sprintf("%d that %s cannot see", len(hidden), viewer.Username)})
		}

		// References to objects that no longer exist.
		stale := map[string]bool{}
		for _, r := range refs {
			if r.Kind != kind || known[r.ObjectID] {
				continue
			}
			what := fmt.Sprintf("id %d", r.ObjectID)
			if r.ByName {
				what = fmt.Sprintf("%q", r.Name)
			}
			msg := fmt.Sprintf("%s refers to %s, which does not exist", r.Label, what)
			if !stale[msg] {
				stale[msg] = true
				findings = append(findings, tidyFinding{Kind: kind, Type: "stale_reference", IDs: []int{}, Detail: msg})
			}
		}
	}
	return rows, findings, summary
}
