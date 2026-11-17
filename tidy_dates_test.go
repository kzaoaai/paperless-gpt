package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTidyReingestOrigin(t *testing.T) {
	cases := []struct {
		id      int
		name    string
		origin  string
		sources []int
	}{
		{2255, "96_1-1.pdf", "split", []int{96}},
		{2256, "96_2-14.pdf", "split", []int{96}},
		{300, "12_15_merged.pdf", "merge", []int{12, 15}},
		{300, "96_edit_0.pdf", "edit", []int{96}},
		{300, "scan_1-2.pdf", "", nil},
		{300, "2024_1-2.pdf", "", nil},         // 2024 is not an earlier document
		{300, "96_rotated.pdf", "", nil},       // a new version of 96, not a new document
		{300, "Invoice (Merged).pdf", "", nil}, // a scanner app's name
		{300, "", "", nil},
	}
	for _, c := range cases {
		origin, sources := tidyReingestOrigin(tidyDoc{ID: c.id, OriginalFileName: c.name})
		assert.Equal(t, c.origin, origin, c.name)
		assert.Equal(t, c.sources, sources, c.name)
	}
	// The joined ids are cut at 100 characters: an id cut short is dropped,
	// one that ends on the cut is kept.
	origin, sources := tidyReingestOrigin(tidyDoc{ID: 99999, OriginalFileName: strings.Repeat("12345_", 20)[:100] + "_merged.pdf"})
	assert.Equal(t, "merge", origin)
	assert.Len(t, sources, 16)
	_, sources = tidyReingestOrigin(tidyDoc{ID: 99999, OriginalFileName: strings.Repeat("1234_", 20) + "_merged.pdf"})
	assert.Len(t, sources, 20)
}

func TestTidyDateRules(t *testing.T) {
	docs := []tidyDoc{
		{ID: 96, Title: "source", Created: "2017-06-07", Added: "2023-04-06T11:09:41+03:00"},
		// A piece of 96 dated when it was split: flagged.
		{ID: 200, Title: "late piece", OriginalFileName: "96_1-1.pdf", Created: "2017-06-07", Added: "2026-11-17T04:20:26+02:00"},
		// A piece given its source's date: fine.
		{ID: 201, Title: "fixed piece", OriginalFileName: "96_2-2.pdf", Created: "2017-06-21", Added: "2023-04-06T11:09:41+03:00"},
		// Split within a day of its source: ordinary intake.
		{ID: 202, Title: "same-day piece", OriginalFileName: "96_3-3.pdf", Created: "2017-06-21", Added: "2023-04-06T18:00:00+03:00"},
		// Its source is in the trash.
		{ID: 203, Title: "piece of a trashed one", OriginalFileName: "50_1-2.pdf", Created: "2020-01-01", Added: "2026-11-17T04:20:26+02:00"},
		// A merge takes the newest of its sources (96 and 150).
		{ID: 150, Title: "second source", Created: "2024-01-01", Added: "2024-05-01T10:00:00+03:00"},
		{ID: 204, Title: "merged", OriginalFileName: "96_150_merged.pdf", Created: "2024-01-01", Added: "2024-05-01T12:00:00+03:00"},
		// Its source is gone but was seen on an earlier pass.
		{ID: 205, Title: "remembered", OriginalFileName: "60_1-1.pdf", Created: "2020-01-01", Added: "2026-11-17T04:20:26+02:00"},
		// Its source is gone and was never seen: nothing to compare with.
		{ID: 206, Title: "unknown", OriginalFileName: "61_1-1.pdf", Created: "2020-01-01", Added: "2026-11-17T04:20:26+02:00"},
		// Created dates against the added day, in the server's own offset.
		{ID: 300, Title: "three days after", Created: "2024-03-13", Added: "2024-03-10T09:00:00+02:00"},
		{ID: 301, Title: "next day", Created: "2024-03-11", Added: "2024-03-10T23:30:00+02:00"},
		{ID: 302, Title: "utc date is the day before", Created: "2024-03-10", Added: "2024-03-10T00:30:00+02:00"},
		{ID: 303, Title: "old paper", Created: "1999-01-01", Added: "2024-03-10T09:00:00+02:00"},
	}
	trash := map[int]string{50: "2023-05-01T08:00:00+03:00"}
	remembered := map[int]string{205: "2023-06-01T08:00:00+03:00"}
	items, remember, unchecked := tidyBuildDateItems(docs, trash, remembered)

	by := map[int]tidyDateItem{}
	for _, it := range items {
		by[it.DocID] = it
	}
	assert.Len(t, items, 4)
	assert.Equal(t, tidyDateItem{DocID: 200, Title: "late piece", Created: "2017-06-07", Added: "2026-11-17T04:20:26+02:00",
		Rule: tidyDateReingest, Origin: "split", SourceIDs: []int{96}, SourceAdded: "2023-04-06T11:09:41+03:00", SourceState: "document"}, by[200])
	assert.Equal(t, "trash", by[203].SourceState)
	assert.Equal(t, "2023-05-01T08:00:00+03:00", by[203].SourceAdded)
	assert.Equal(t, "remembered", by[205].SourceState)
	assert.Equal(t, tidyDateAfterArrival, by[300].Rule)
	assert.Equal(t, 3, by[300].Days)
	assert.Equal(t, []int{206}, unchecked)
	// Every piece whose source could be read is remembered, flagged or not.
	assert.Equal(t, map[int]string{
		200: "2023-04-06T11:09:41+03:00", 201: "2023-04-06T11:09:41+03:00", 202: "2023-04-06T11:09:41+03:00",
		203: "2023-05-01T08:00:00+03:00", 204: "2024-05-01T10:00:00+03:00",
	}, remember)

	script := tidyDateFixScript(items)
	assert.Contains(t, script, `200: "2023-04-06T11:09:41+03:00",  # split of 96`)
	assert.Contains(t, script, "bulk_update_documents(list(fix))")
	assert.NotContains(t, script, "300:")
	assert.Empty(t, tidyDateFixScript([]tidyDateItem{by[300]}))
}

func TestTidyDateChecksIgnoreAndRemember(t *testing.T) {
	f := newFakePaperless(t)
	f.addDoc(tidyDoc{ID: 96, Title: "source", Created: "2017-06-07", Added: "2023-04-06T11:09:41+03:00"})
	f.addDoc(tidyDoc{ID: 200, Title: "late piece", OriginalFileName: "96_1-1.pdf", Created: "2017-06-07", Added: "2026-11-17T04:20:26+02:00"})
	f.addDoc(tidyDoc{ID: 300, Title: "three days after", Created: "2024-03-13", Added: "2024-03-10T09:00:00+02:00"})
	s := newTestTidy(t, f)
	ctx := context.Background()

	inv, err := s.inventory(ctx, true)
	require.NoError(t, err)
	counts := map[string]int{}
	for _, h := range inv.Hygiene {
		counts[h.Key] = h.Count
	}
	assert.Equal(t, 1, counts["reingest_added"])
	assert.Equal(t, 1, counts["dated_after_arrival"])

	// Ignoring one takes it off the list and the count, and names the rule.
	n, err := s.ignoreDates([]tidyDateIgnoreItem{{DocID: 300, Rule: tidyDateAfterArrival, Title: "three days after"}, {DocID: 300, Rule: "nonsense"}})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	checks, err := s.dateChecks(ctx, nil)
	require.NoError(t, err)
	require.Len(t, checks.Items, 1)
	assert.Equal(t, 200, checks.Items[0].DocID)
	require.Len(t, checks.Ignored, 1)
	assert.True(t, checks.Ignored[0].Flagged)
	assert.Equal(t, "three days after", checks.Ignored[0].Current)
	inv, err = s.inventory(ctx, false) // the ignore dropped the cache
	require.NoError(t, err)
	for _, h := range inv.Hygiene {
		if h.Key == "dated_after_arrival" {
			assert.Equal(t, 0, h.Count)
		}
	}

	// The source goes to the trash, then leaves it: the piece stays flagged
	// with the date Tidy remembered.
	f.mu.Lock()
	src := *f.docs[96]
	delete(f.docs, 96)
	f.trash = []tidyDoc{src}
	f.mu.Unlock()
	checks, err = s.dateChecks(ctx, nil)
	require.NoError(t, err)
	require.Len(t, checks.Items, 1)
	assert.Equal(t, "trash", checks.Items[0].SourceState)
	f.mu.Lock()
	f.trash = nil
	f.mu.Unlock()
	checks, err = s.dateChecks(ctx, nil)
	require.NoError(t, err)
	require.Len(t, checks.Items, 1)
	assert.Equal(t, "remembered", checks.Items[0].SourceState)
	assert.Equal(t, "2023-04-06T11:09:41+03:00", checks.Items[0].SourceAdded)
	assert.Contains(t, checks.FixScript, "200:")
	assert.Empty(t, checks.Unchecked)

	// Once the piece carries its source's date it is no longer listed.
	f.mu.Lock()
	f.docs[200].Added = "2023-04-06T11:09:41+03:00"
	f.mu.Unlock()
	checks, err = s.dateChecks(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, checks.Items)
	assert.Empty(t, checks.FixScript)

	n, err = s.unignoreDates([]tidyDateIgnoreItem{{DocID: 300, Rule: tidyDateAfterArrival}})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	checks, err = s.dateChecks(ctx, nil)
	require.NoError(t, err)
	require.Len(t, checks.Items, 1)
	assert.Equal(t, 300, checks.Items[0].DocID)
	assert.Empty(t, checks.Ignored)
	assert.Empty(t, f.writes, "the date checks never write to paperless-ngx")
}
