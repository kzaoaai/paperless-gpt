package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTidyTitleLooksRaw(t *testing.T) {
	cases := []struct {
		title, file string
		raw         bool
	}{
		{"Scan_20230412", "Scan_20230412.pdf", true},
		{"ID Adam Passport 2025", "ID Adam Passport 2025.pdf", true}, // the file name; a person may still keep it
		{"41ead9a1-381b-4cbb-95e7-2f080ff63b4f", "x.pdf", true},
		{"Parsons Background Screening Results.pdf", "", true},
		{"Paystub_Sample_051123", "", true},
		{"Paychecks", "", true},
		{"011217", "", true},
		{"PDF document", "", true},
		{"", "", true},
		{"LA County - Property Tax Payment Confirmation - 3930488193", "receipt.pdf", false},
		{"Costco Receipt", "IMG_0001.jpg", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.raw, tidyTitleLooksRaw(c.title, c.file), "%q (file %q)", c.title, c.file)
	}
}

func TestTidyTitleRecheck(t *testing.T) {
	f := newFakePaperless(t)
	f.addDoc(tidyDoc{ID: 1, Added: "2026-01-01T00:00:00Z", OriginalFileName: "scan_001.pdf"})
	f.addDoc(tidyDoc{ID: 2, Added: "2026-02-01T00:00:00Z"})
	f.addDoc(tidyDoc{ID: 3, Added: "2026-03-01T00:00:00Z"})
	f.addDoc(tidyDoc{ID: 4, Added: "2026-04-01T00:00:00Z"})
	f.titles[1], f.titles[2], f.titles[3], f.titles[4] = "scan_001", "Chase - Statement - 2026-01", "Paychecks", "Bank - Letter - 2026"
	s := newTestTidy(t, f)
	var asked []int
	s.titleSuggest = func(ctx context.Context, id int) (string, string, error) {
		asked = append(asked, id)
		return f.titles[id], "  Suggested   " + strings.Repeat("x", 200), nil
	}
	ctx := context.Background()

	st, err := s.titleStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, st.Candidates, "1 and 3 look raw; 2 and 4 read as titles")

	_, err = s.ignoreTitles([]tidyFillIgnoreItem{{DocID: 3, Title: "Paychecks"}})
	require.NoError(t, err)
	run, err := s.startTitles(ctx, nil, 0)
	require.NoError(t, err)
	waitSplit(t, s)
	assert.Equal(t, []int{1}, asked, "an ignored title is not asked about")

	view, err := s.titleView(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, view.Items, 1)
	it := view.Items[0]
	assert.Equal(t, "scan_001", it.Current)
	assert.Len(t, []rune(it.Suggested), tidyTitleMax, "cut to what paperless-ngx keeps")
	assert.True(t, strings.HasPrefix(it.Suggested, "Suggested x"), "spaces collapsed")
	assert.False(t, it.Changed)

	// Explicit ids are asked about even when ignored.
	asked = nil
	_, err = s.startTitles(ctx, []int{3, 2}, 0)
	require.NoError(t, err)
	waitSplit(t, s)
	assert.ElementsMatch(t, []int{2, 3}, asked)

	// Apply an edited title to 1; 99 is not in the run, so it is skipped.
	a, err := s.applyTitles(ctx, run.ID, []tidyTitleApplyItem{{DocID: 1, Title: " Acme - Invoice - 42 "}, {DocID: 99, Title: "x"}})
	require.NoError(t, err)
	assert.Equal(t, "done", a.Status)
	assert.Contains(t, a.Summary, "Retitled 1 documents; skipped 1")
	assert.Equal(t, "Acme - Invoice - 42", f.titles[1])
	view, err = s.titleView(ctx, run.ID)
	require.NoError(t, err)
	assert.True(t, view.Items[0].Changed)

	// Applying again is refused: the title is no longer what the run saw.
	_, err = s.applyTitles(ctx, run.ID, []tidyTitleApplyItem{{DocID: 1, Title: "Other"}})
	require.Error(t, err)

	var d tidyActionDetail
	require.NoError(t, json.Unmarshal([]byte(a.Detail), &d))
	assert.Equal(t, []tidyTitleChange{{ID: 1, Before: "scan_001", After: "Acme - Invoice - 42"}}, d.Titles)

	// Undo puts the old title back; a hand edit since makes it ask first.
	f.titles[1] = "Edited by hand"
	f.docs[1].Title = "Edited by hand"
	_, err = s.undo(ctx, a.ID, false)
	require.Error(t, err)
	f.titles[1] = "Acme - Invoice - 42"
	f.docs[1].Title = "Acme - Invoice - 42"
	_, err = s.undo(ctx, a.ID, false)
	require.NoError(t, err)
	assert.Equal(t, "scan_001", f.titles[1])
}
