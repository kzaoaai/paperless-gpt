package main

// Tidy: date checks. Two things a document's dates can show:
//
//   - re-ingested: paperless-ngx's split, merge and "edit PDF into new
//     documents" consume their output as brand-new documents, so a piece cut
//     out of a document added years ago is dated as added today and tops every
//     "latest" view. The upload file name says where it came from
//     ("96_1-1.pdf", "12_15_merged.pdf", "96_edit_0.pdf"), so the piece's added
//     date can be compared with its source's.
//   - dated after arrival: a created date (the document's own date) later than
//     the day it was added, which a real document date cannot be.
//
// A created date that merely differs from the added date says nothing: most of
// an archive is old paper scanned later.
//
// Both are read-only here. paperless-ngx's API cannot write `added` (the model
// field is not editable), so the re-ingested list comes with a script for the
// paperless-ngx shell instead of a button.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

const (
	tidyDateReingest     = "reingest"
	tidyDateAfterArrival = "after_arrival"

	// A piece made within a day of its source is ordinary intake (scan, then
	// split), not a correction to an old document.
	tidyDateReingestSlack = 24 * time.Hour
	// One day is what a time-zone difference between the document and the
	// server explains.
	tidyDateArrivalMinDays = 2
)

// TidyDateIgnore is a document a person chose to leave as it is for one rule.
type TidyDateIgnore struct {
	DocID     int       `gorm:"primaryKey;autoIncrement:false" json:"doc_id"`
	Rule      string    `gorm:"primaryKey" json:"rule"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// TidyDateSource remembers the added date a split or merged document should
// carry, from when its source could still be read: paperless-ngx empties its
// trash after a while, and the date goes with the source.
type TidyDateSource struct {
	DocID     int    `gorm:"primaryKey;autoIncrement:false"`
	Added     string // RFC 3339, as paperless-ngx gave it
	UpdatedAt time.Time
}

type tidyDateItem struct {
	DocID   int    `json:"doc_id"`
	Title   string `json:"title"`
	Created string `json:"created"`
	Added   string `json:"added"`
	Rule    string `json:"rule"`
	// after_arrival: how many days the created date lies after the added day.
	Days int `json:"days,omitempty"`
	// reingest: how the document was made, from which documents, the added
	// date it should carry and where that date was read ("document", "trash",
	// "remembered" once the source is gone, or "estimated" when the source
	// was gone before it was ever seen: then the date is that of the next
	// document added after the source, EstimatedFrom).
	Origin        string `json:"origin,omitempty"`
	SourceIDs     []int  `json:"source_ids,omitempty"`
	SourceAdded   string `json:"source_added,omitempty"`
	SourceState   string `json:"source_state,omitempty"`
	EstimatedFrom int    `json:"estimated_from,omitempty"`
}

type tidyDateIgnored struct {
	TidyDateIgnore
	Current string `json:"current"`
	Gone    bool   `json:"gone"`
	// Flagged: the rule would still list the document today.
	Flagged bool `json:"flagged"`
}

type tidyDateChecks struct {
	Items   []tidyDateItem    `json:"items"`
	Ignored []tidyDateIgnored `json:"ignored"`
	// Split or merged documents whose source is gone, was never seen, and has
	// no document added between it and them to estimate its date from.
	Unchecked []int `json:"unchecked"`
	// FixScript sets `added` on every listed re-ingested document; it is run in
	// the paperless-ngx shell.
	FixScript string   `json:"fix_script,omitempty"`
	Warnings  []string `json:"warnings"`
}

type tidyDateIgnoreItem struct {
	DocID int    `json:"doc_id"`
	Rule  string `json:"rule"`
	Title string `json:"title"`
}

var (
	tidySplitName = regexp.MustCompile(`^(\d+)_\d+-\d+\.pdf$`)
	tidyEditName  = regexp.MustCompile(`^(\d+)_edit_\d+\.pdf$`)
	tidyMergeName = regexp.MustCompile(`^(\d[\d_]*)_merged\.pdf$`)
)

// tidyReingestOrigin reads how paperless-ngx made a document from its upload
// file name. Sources are the documents it was made from; only ids below the
// document's own count, since a source exists before its pieces.
func tidyReingestOrigin(d tidyDoc) (origin string, sources []int) {
	name := strings.TrimSpace(d.OriginalFileName)
	var ids string
	if m := tidySplitName.FindStringSubmatch(name); m != nil {
		origin, ids = "split", m[1]
	} else if m := tidyEditName.FindStringSubmatch(name); m != nil {
		origin, ids = "edit", m[1]
	} else if m := tidyMergeName.FindStringSubmatch(name); m != nil {
		origin, ids = "merge", m[1]
		// paperless-ngx cuts the joined ids at 100 characters, so the last
		// one may be incomplete unless the cut fell on a separator.
		if len(ids) >= 100 && !strings.HasSuffix(ids, "_") {
			if i := strings.LastIndex(ids, "_"); i > 0 {
				ids = ids[:i]
			}
		}
	} else {
		return "", nil
	}
	for _, part := range strings.Split(ids, "_") {
		if id, err := strconv.Atoi(part); err == nil && id > 0 && id < d.ID {
			sources = append(sources, id)
		}
	}
	if len(sources) == 0 {
		return "", nil
	}
	return origin, sources
}

func tidyParseAdded(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// tidyDaysAfterArrival is how many days the created date lies after the day
// the document was added, in the server's own time zone (the offset `added`
// carries). Zero or less means the dates are in order.
func tidyDaysAfterArrival(d tidyDoc) int {
	added, ok := tidyParseAdded(d.Added)
	if !ok || len(d.Created) < 10 {
		return 0
	}
	created, err := time.Parse("2006-01-02", d.Created[:10])
	if err != nil {
		return 0
	}
	y, m, day := added.Date()
	addedDay := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	return int(created.Sub(addedDay).Hours() / 24)
}

// tidyDatedDoc is a document that was consumed in the ordinary way, with its
// added date: what a gone source's date is estimated from.
type tidyDatedDoc struct {
	id    int
	added time.Time
	text  string
}

// tidyEstimateSourceAdded dates a source that no longer exists. paperless-ngx
// numbers documents in the order it consumed them, so the source was added no
// later than the next document after it: plain[i] is the first one above the
// source. That document must come before the piece, or it says nothing, and
// must not be dated before the document below the source, which would mean
// someone rewrote the dates around it. plain is sorted by id.
func tidyEstimateSourceAdded(plain []tidyDatedDoc, source, piece int) (est tidyDatedDoc, ok bool) {
	i := sort.Search(len(plain), func(i int) bool { return plain[i].id > source })
	if i == len(plain) || plain[i].id >= piece {
		return est, false
	}
	if i > 0 && plain[i-1].added.Sub(plain[i].added) > tidyDateReingestSlack {
		return est, false
	}
	return plain[i], true
}

// tidyBuildDateItems applies both rules to every document. trash maps a
// trashed document's id to its added date; remembered is what earlier passes
// saw. remember is what this pass can vouch for, to be stored.
func tidyBuildDateItems(docs []tidyDoc, trash map[int]string, remembered map[int]string) (items []tidyDateItem, remember map[int]string, unchecked []int) {
	live := make(map[int]string, len(docs))
	var plain []tidyDatedDoc
	for _, d := range docs {
		live[d.ID] = d.Added
		// A split or merged piece may carry a corrected date, so it is no
		// witness for when its neighbours arrived.
		if origin, _ := tidyReingestOrigin(d); origin == "" {
			if t, ok := tidyParseAdded(d.Added); ok {
				plain = append(plain, tidyDatedDoc{id: d.ID, added: t, text: d.Added})
			}
		}
	}
	sort.Slice(plain, func(i, j int) bool { return plain[i].id < plain[j].id })
	remember = map[int]string{}
	for _, d := range docs {
		if days := tidyDaysAfterArrival(d); days >= tidyDateArrivalMinDays {
			items = append(items, tidyDateItem{DocID: d.ID, Title: d.Title, Created: d.Created, Added: d.Added,
				Rule: tidyDateAfterArrival, Days: days})
		}
		origin, sources := tidyReingestOrigin(d)
		if origin == "" {
			continue
		}
		// The date a piece should carry is its source's; a merge takes the
		// newest of its sources, the day the merged document was complete.
		var want time.Time
		wantText, state := "", ""
		for _, id := range sources {
			text, where := live[id], "document"
			if text == "" {
				text, where = trash[id], "trash"
			}
			if t, ok := tidyParseAdded(text); ok && (wantText == "" || t.After(want)) {
				want, wantText, state = t, text, where
			}
		}
		if wantText != "" {
			remember[d.ID] = wantText
		} else if text := remembered[d.ID]; text != "" {
			if t, ok := tidyParseAdded(text); ok {
				want, wantText, state = t, text, "remembered"
			}
		}
		// No source was ever seen: estimate. Not remembered, since it is
		// worked out again from whatever documents exist.
		from := 0
		if wantText == "" {
			for _, id := range sources {
				if est, ok := tidyEstimateSourceAdded(plain, id, d.ID); ok && (wantText == "" || est.added.After(want)) {
					want, wantText, state, from = est.added, est.text, "estimated", est.id
				}
			}
		}
		if wantText == "" {
			unchecked = append(unchecked, d.ID)
			continue
		}
		added, ok := tidyParseAdded(d.Added)
		if !ok || added.Sub(want) <= tidyDateReingestSlack {
			continue
		}
		items = append(items, tidyDateItem{DocID: d.ID, Title: d.Title, Created: d.Created, Added: d.Added,
			Rule: tidyDateReingest, Origin: origin, SourceIDs: sources, SourceAdded: wantText, SourceState: state, EstimatedFrom: from})
	}
	return items, remember, unchecked
}

// tidyDateFixScript is the paperless-ngx shell script that gives each listed
// re-ingested document its source's added date and refreshes its index entry.
func tidyDateFixScript(items []tidyDateItem) string {
	var lines []string
	for _, it := range items {
		if it.Rule != tidyDateReingest || it.SourceAdded == "" {
			continue
		}
		ids := make([]string, len(it.SourceIDs))
		for i, id := range it.SourceIDs {
			ids[i] = strconv.Itoa(id)
		}
		note := ""
		if it.SourceState == "estimated" {
			note = fmt.Sprintf(" (gone; estimated from %d, the next document added)", it.EstimatedFrom)
		}
		lines = append(lines, fmt.Sprintf("    %d: %q,  # %s of %s%s", it.DocID, it.SourceAdded, it.Origin, strings.Join(ids, ", "), note))
	}
	if len(lines) == 0 {
		return ""
	}
	return "# Run in the paperless-ngx shell, as the paperless user:\n" +
		"#   docker exec -i -u paperless <webserver container> python3 manage.py shell\n" +
		"from django.utils.dateparse import parse_datetime\n" +
		"from documents.models import Document\n" +
		"from documents.tasks import bulk_update_documents\n" +
		"fix = {\n" + strings.Join(lines, "\n") + "\n}\n" +
		"for pk, added in fix.items():\n" +
		"    Document.objects.filter(pk=pk).update(added=parse_datetime(added))\n" +
		"\n" +
		"bulk_update_documents(list(fix))\n"
}

// tidyFetchTrash maps each trashed document to its added date. The trash list
// ignores `fields`, so every entry comes with its full text: fine for a trash
// paperless-ngx empties by itself.
func tidyFetchTrash(ctx context.Context, c tidyHTTP) (map[int]string, error) {
	raws, err := tidyListAll(ctx, c, "api/trash/?page_size=100")
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(raws))
	for _, raw := range raws {
		var d struct {
			ID    int    `json:"id"`
			Added string `json:"added"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		out[d.ID] = d.Added
	}
	return out, nil
}

func tidyDateKey(rule string, id int) string { return rule + ":" + strconv.Itoa(id) }

// dateChecks runs both rules over docs (fetched when nil), leaves out what a
// person ignored and stores the source dates it could read.
func (s *tidyService) dateChecks(ctx context.Context, docs []tidyDoc) (*tidyDateChecks, error) {
	if docs == nil {
		var err error
		if docs, err = tidyFetchDocs(ctx, s.client, ""); err != nil {
			return nil, err
		}
	}
	out := &tidyDateChecks{Items: []tidyDateItem{}, Ignored: []tidyDateIgnored{}, Unchecked: []int{}, Warnings: []string{}}
	trash, err := tidyFetchTrash(ctx, s.client)
	if err != nil {
		out.Warnings = append(out.Warnings, "Could not read paperless-ngx's trash, so split or merged documents whose source is there were not checked: "+err.Error())
		trash = map[int]string{}
	}
	remembered := map[int]string{}
	var ignores []TidyDateIgnore
	if s.db != nil {
		var rows []TidyDateSource
		if err := s.db.Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			remembered[r.DocID] = r.Added
		}
		if err := s.db.Order("created_at desc, doc_id desc").Find(&ignores).Error; err != nil {
			return nil, err
		}
	}
	items, remember, unchecked := tidyBuildDateItems(docs, trash, remembered)
	if s.db != nil {
		for id, added := range remember {
			if remembered[id] == added {
				continue
			}
			row := TidyDateSource{DocID: id, Added: added}
			if err := s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
				return nil, err
			}
		}
	}
	ignored := make(map[string]bool, len(ignores))
	for _, r := range ignores {
		ignored[tidyDateKey(r.Rule, r.DocID)] = true
	}
	flagged := make(map[string]bool, len(items))
	for _, it := range items {
		key := tidyDateKey(it.Rule, it.DocID)
		flagged[key] = true
		if !ignored[key] {
			out.Items = append(out.Items, it)
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].DocID > out.Items[j].DocID })
	titles := make(map[int]string, len(docs))
	for _, d := range docs {
		titles[d.ID] = d.Title
	}
	for _, r := range ignores {
		it := tidyDateIgnored{TidyDateIgnore: r, Flagged: flagged[tidyDateKey(r.Rule, r.DocID)]}
		if title, ok := titles[r.DocID]; ok {
			it.Current = title
		} else {
			it.Gone = true
		}
		out.Ignored = append(out.Ignored, it)
	}
	if unchecked != nil {
		out.Unchecked = unchecked
	}
	out.FixScript = tidyDateFixScript(out.Items)
	return out, nil
}

// tidyDateHygiene is the two counts the page's health row shows.
func tidyDateHygiene(checks *tidyDateChecks) []tidyHygieneItem {
	var reingest, after int
	for _, it := range checks.Items {
		if it.Rule == tidyDateReingest {
			reingest++
		} else {
			after++
		}
	}
	return []tidyHygieneItem{
		{Key: "reingest_added", Label: "Split or merged, dated as new", Count: reingest},
		{Key: "dated_after_arrival", Label: "Dated after they arrived", Count: after},
	}
}

func tidyValidDateRule(rule string) bool {
	return rule == tidyDateReingest || rule == tidyDateAfterArrival
}

func (s *tidyService) ignoreDates(items []tidyDateIgnoreItem) (int, error) {
	if s.db == nil {
		return 0, tidyRefuse("no database")
	}
	added := 0
	for _, it := range items {
		if it.DocID <= 0 || !tidyValidDateRule(it.Rule) {
			continue
		}
		row := TidyDateIgnore{DocID: it.DocID, Rule: it.Rule, Title: strings.TrimSpace(it.Title)}
		res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			return added, res.Error
		}
		added += int(res.RowsAffected)
	}
	s.invalidate()
	return added, nil
}

func (s *tidyService) unignoreDates(items []tidyDateIgnoreItem) (int, error) {
	if s.db == nil {
		return 0, tidyRefuse("no database")
	}
	removed := 0
	for _, it := range items {
		res := s.db.Where("doc_id = ? AND rule = ?", it.DocID, it.Rule).Delete(&TidyDateIgnore{})
		if res.Error != nil {
			return removed, res.Error
		}
		removed += int(res.RowsAffected)
	}
	s.invalidate()
	return removed, nil
}
