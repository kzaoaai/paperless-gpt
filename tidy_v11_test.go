package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTidyHygieneCounts(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	old := "2026-10-01T00:00:00Z"
	recent := "2026-11-15T06:00:00Z"
	tags := []tidyObject{
		{ID: 1, Name: "Inbox", IsInboxTag: true},
		{ID: 2, Name: "gpt-auto"},
		{ID: 3, Name: "gpt-failed"},
		{ID: 4, Name: "Taxes"},
	}
	docs := []tidyDoc{
		{ID: 1, Added: old, Tags: []int{1}, Correspondent: ip(5), DocumentType: ip(6)},    // stale inbox
		{ID: 2, Added: recent, Tags: []int{1}, Correspondent: ip(5)},                      // fresh inbox, no type
		{ID: 3, Added: old, Tags: []int{2}, DocumentType: ip(6)},                          // stuck in the queue, no correspondent
		{ID: 4, Added: recent, Tags: []int{2}, Correspondent: ip(5), DocumentType: ip(6)}, // just queued
		{ID: 5, Added: old, Tags: []int{3, 4}, Correspondent: ip(5), DocumentType: ip(6)},
		{ID: 6, Added: old, Tags: []int{}, Correspondent: ip(5), DocumentType: ip(6)},
	}
	items := tidyBuildHygiene(docs, tags, tidyConfigRefs{AutoTag: "GPT-AUTO", OCRTag: "", FailTag: "gpt-failed"}, now)
	got := map[string]tidyHygieneItem{}
	for _, it := range items {
		got[it.Key] = it
	}
	assert.Equal(t, 1, got["no_correspondent"].Count)
	assert.Equal(t, 1, got["no_document_type"].Count)
	assert.Equal(t, 1, got["no_tags"].Count)
	assert.Equal(t, 1, got["stale_inbox"].Count)
	assert.Equal(t, "is_in_inbox=1&added__date__lt=2026-11-01", got["stale_inbox"].Query)
	assert.Equal(t, 1, got["stuck_queue"].Count, "queue tags match by name, ignoring case")
	assert.Equal(t, "tags__id__in=2&added__date__lt=2026-11-14", got["stuck_queue"].Query)
	assert.Equal(t, 1, got["failed"].Count)
	assert.Equal(t, "tags__id__all=3", got["failed"].Query)
}

func TestTidyFieldAudit(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	fields := []tidyObject{
		{ID: 24, Name: "Tax Year", DataType: "string", Raw: raw(`{"id":24,"name":"Tax Year","data_type":"string"}`)},
		{ID: 22, Name: "Category", DataType: "select", Raw: raw(`{"id":22,"name":"Category","data_type":"select",
			"extra_data":{"select_options":[{"id":"a1","label":"Bills"},{"id":"b2","label":"Medical"},{"id":"c3","label":"Unused one"}]}}`)},
		{ID: 10, Name: "Amount", DataType: "monetary", Raw: raw(`{"id":10,"name":"Amount","data_type":"monetary"}`)},
		{ID: 23, Name: "Related", DataType: "documentlink", Raw: raw(`{"id":23,"name":"Related","data_type":"documentlink"}`)},
	}
	cf := func(pairs ...any) []tidyFieldValue {
		var out []tidyFieldValue
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, tidyFieldValue{Field: pairs[i].(int), Value: raw(pairs[i+1].(string))})
		}
		return out
	}
	docs := []tidyDoc{
		{ID: 1, CustomFields: cf(24, `"2025, 2026"`, 22, `"a1"`, 10, `"USD100.00"`, 23, `[5,6]`)},
		{ID: 2, CustomFields: cf(24, `"2025,2026"`, 22, `"a1"`, 10, `"42.50"`)},
		{ID: 3, CustomFields: cf(24, `"2024"`, 22, `"b2"`, 10, `null`)},
		{ID: 4, CustomFields: cf(24, `""`, 23, `[]`)},
	}
	audit := tidyBuildFieldAudit(docs, fields)
	by := map[string]tidyFieldAudit{}
	for _, a := range audit {
		by[a.Name] = a
	}
	tax := by["Tax Year"]
	assert.Equal(t, 4, tax.DocCount)
	assert.Equal(t, 1, tax.EmptyCount)
	assert.Equal(t, 3, tax.Distinct)
	require.Len(t, tax.Variants, 1)
	assert.ElementsMatch(t, []string{"2025, 2026", "2025,2026"}, []string{tax.Variants[0][0].Value, tax.Variants[0][1].Value})

	cat := by["Category"]
	assert.Equal(t, []tidyValueCount{{Value: "Bills", Count: 2}, {Value: "Medical", Count: 1}}, cat.Top, "select ids become labels")
	assert.Equal(t, []string{"Unused one"}, cat.UnusedOptions)

	amount := by["Amount"]
	assert.Equal(t, 1, amount.NoCurrency)
	assert.Equal(t, 1, amount.EmptyCount)

	rel := by["Related"]
	assert.Equal(t, 1, rel.EmptyCount)
	assert.Equal(t, []tidyValueCount{{Value: "(links)", Count: 1}}, rel.Top)
}

// waitSplit waits for the background run to finish.
func waitSplit(t *testing.T, s *tidyService) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.splitMu.Lock()
		running := s.splitCancel != nil
		s.splitMu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("split run did not finish")
}

func TestTidySplitApplyAndUndo(t *testing.T) {
	f := newFakePaperless(t)
	f.users = []tidyUser{{ID: 3, Username: "mcp-reader"}}
	f.addObject("correspondents", 40, "Real Estate", map[string]any{"owner": 2.0})
	f.addObject("correspondents", 3, "City of Springfield", nil)
	f.addObject("correspondents", 7, "Acme Hardware", nil)
	titles := map[int]string{1: "Water bill", 2: "Water bill 2", 3: "HOA dues", 4: "Deed", 5: "Mystery", 6: "Paint receipt"}
	for id, title := range titles {
		f.titles[id] = title
		f.addDoc(tidyDoc{ID: id, Correspondent: ip(40)})
	}
	s := newTestTidy(t, f)
	var seenNames []string
	s.suggest = func(ctx context.Context, title, content string, names []string) (string, error) {
		seenNames = names
		switch {
		case strings.HasPrefix(title, "Water"):
			return "city of springfield", nil // case differs: resolves to 3
		case title == "HOA dues":
			return "Maple Court HOA", nil // new
		case title == "Paint receipt":
			return "Acme Hardware Inc.", nil // new, but looks like 7
		case title == "Deed":
			return "Real Estate", nil
		}
		return "Unknown", nil
	}
	ctx := context.Background()

	_, err := s.applySplit(ctx, 99, nil)
	require.Error(t, err)

	run, err := s.startSplit(ctx, 40, true)
	require.NoError(t, err)
	waitSplit(t, s)
	assert.NotContains(t, seenNames, "Real Estate", "exclude_self keeps the bucket out of the choices")
	assert.Empty(t, f.writes, "a run changes nothing")

	view, err := s.splitView(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "done", view.Run.Status)
	assert.Equal(t, 6, view.Run.Done)
	groups := map[string]tidySplitGroup{}
	for _, g := range view.Groups {
		groups[g.Key] = g
	}
	require.Contains(t, groups, "id:3")
	assert.Len(t, groups["id:3"].Docs, 2)
	require.Contains(t, groups, "new:maple court hoa")
	assert.True(t, groups["new:maple court hoa"].New)
	assert.Equal(t, "Acme Hardware", groups["new:acme hardware inc."].Similar)
	assert.Len(t, groups["stay"].Docs, 2, "the bucket's own name and Unknown stay")
	assert.True(t, view.Groups[len(view.Groups)-1].Stay, "stay sorts last")

	a, err := s.applySplit(ctx, run.ID, []tidySplitApplyGroup{
		{DocIDs: []int{1, 2}, TargetID: 3},
		{DocIDs: []int{3}, NewName: "Maple Court HOA"},
		{DocIDs: []int{6}, TargetID: 7}, // AA chose the existing one over the new name
	})
	require.NoError(t, err)
	assert.Equal(t, "done", a.Status)
	assert.Equal(t, 3, *f.docs[1].Correspondent)
	assert.Equal(t, 7, *f.docs[6].Correspondent)
	assert.Equal(t, 40, *f.docs[4].Correspondent, "unticked documents stay")
	var hoaID int
	for id, o := range f.objects["correspondents"] {
		if o["name"] == "Maple Court HOA" {
			hoaID = id
		}
	}
	require.NotZero(t, hoaID)
	assert.Equal(t, hoaID, *f.docs[3].Correspondent)
	view2 := f.objects["correspondents"][hoaID]["permissions"].(map[string]any)["view"].(map[string]any)
	assert.Equal(t, []int{3}, toInts(view2["users"]), "a new correspondent is visible to the viewer")

	// A second apply skips documents that already left the bucket.
	_, err = s.applySplit(ctx, run.ID, []tidySplitApplyGroup{{DocIDs: []int{1}, TargetID: 3}})
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal)

	f.docs[2].Correspondent = ip(7) // changed by hand since
	_, err = s.undo(ctx, a.ID, false)
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, []int{2}, refusal.DriftedDocs)

	u, err := s.undo(ctx, a.ID, true)
	require.NoError(t, err)
	for _, id := range []int{1, 3, 6} {
		assert.Equal(t, 40, *f.docs[id].Correspondent, "doc %d back on the bucket", id)
	}
	assert.Equal(t, 7, *f.docs[2].Correspondent, "the changed one is left alone")
	assert.Nil(t, f.objects["correspondents"][hoaID], "the correspondent the split created is deleted once empty")
	assert.NotNil(t, f.objects["correspondents"][7], "existing correspondents are never deleted")
	assert.Contains(t, u.UndoNote, "deleted 1 correspondents the split created")
}

func TestTidySplitOneRunAtATimeAndStop(t *testing.T) {
	f := newFakePaperless(t)
	f.addObject("correspondents", 40, "Manufacturer", nil)
	for id := 1; id <= 3; id++ {
		f.titles[id] = "doc"
		f.addDoc(tidyDoc{ID: id, Correspondent: ip(40)})
	}
	s := newTestTidy(t, f)
	release := make(chan struct{})
	s.suggest = func(ctx context.Context, title, content string, names []string) (string, error) {
		select {
		case <-release:
			return "Acme", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	ctx := context.Background()
	run, err := s.startSplit(ctx, 40, true)
	require.NoError(t, err)
	_, err = s.startSplit(ctx, 40, true)
	var refusal *tidyRefusal
	require.ErrorAs(t, err, &refusal, "one run at a time")

	release <- struct{}{} // first document answers
	time.Sleep(50 * time.Millisecond)
	assert.True(t, s.stopSplit())
	waitSplit(t, s)
	stored, results, err := s.loadRun(run.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", stored.Status)
	assert.Len(t, results, 1, "the call cut short by stop is not recorded")
}
