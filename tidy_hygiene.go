package main

// Tidy: archive hygiene counts (documents missing metadata, stuck in a queue
// or failed) and the custom-field value audit. Both are read-only and built
// from the inventory's one pass over every document.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// tidyHygieneItem is one count. Query, when set, is the paperless-ngx
// documents-list query that shows those documents.
type tidyHygieneItem struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
	Query string `json:"query,omitempty"`
}

const (
	tidyInboxStaleAfter = 14 * 24 * time.Hour
	tidyQueueStaleAfter = 24 * time.Hour
)

func tidyTagIDsByName(tags []tidyObject, name string) []int {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	for _, t := range tags {
		if strings.EqualFold(t.Name, name) {
			return []int{t.ID}
		}
	}
	return nil
}

func tidyAddedBefore(d tidyDoc, cutoff time.Time) bool {
	t, err := time.Parse(time.RFC3339Nano, d.Added)
	if err != nil {
		return false
	}
	return t.Before(cutoff)
}

func tidyHasAny(have []int, want []int) bool {
	for _, w := range want {
		for _, h := range have {
			if h == w {
				return true
			}
		}
	}
	return false
}

// tidyBuildHygiene counts documents that need a human: no correspondent,
// type or tags; inbox tags older than two weeks; waiting for paperless-gpt
// for more than a day; fail-tagged by paperless-gpt.
func tidyBuildHygiene(docs []tidyDoc, tags []tidyObject, cfg tidyConfigRefs, now time.Time) []tidyHygieneItem {
	var inbox []int
	for _, t := range tags {
		if t.IsInboxTag {
			inbox = append(inbox, t.ID)
		}
	}
	queue := append(tidyTagIDsByName(tags, cfg.AutoTag), tidyTagIDsByName(tags, cfg.OCRTag)...)
	queue = tidyUniqueSorted(queue)
	fail := tidyTagIDsByName(tags, cfg.FailTag)

	var noCorr, noType, noTags, staleInbox, stuck, failed int
	inboxCut, queueCut := now.Add(-tidyInboxStaleAfter), now.Add(-tidyQueueStaleAfter)
	for _, d := range docs {
		if d.Correspondent == nil {
			noCorr++
		}
		if d.DocumentType == nil {
			noType++
		}
		if len(d.Tags) == 0 {
			noTags++
		}
		if tidyHasAny(d.Tags, inbox) && tidyAddedBefore(d, inboxCut) {
			staleInbox++
		}
		if tidyHasAny(d.Tags, queue) && tidyAddedBefore(d, queueCut) {
			stuck++
		}
		if tidyHasAny(d.Tags, fail) {
			failed++
		}
	}
	items := []tidyHygieneItem{
		{Key: "no_correspondent", Label: "No correspondent", Count: noCorr, Query: "correspondent__isnull=1"},
		{Key: "no_document_type", Label: "No document type", Count: noType, Query: "document_type__isnull=1"},
		{Key: "no_tags", Label: "No tags", Count: noTags, Query: "is_tagged=0"},
	}
	if len(inbox) > 0 {
		items = append(items, tidyHygieneItem{Key: "stale_inbox", Label: "Still in the inbox after two weeks", Count: staleInbox,
			Query: "is_in_inbox=1&added__date__lt=" + inboxCut.Format("2006-01-02")})
	}
	if len(queue) > 0 {
		items = append(items, tidyHygieneItem{Key: "stuck_queue", Label: "Waiting for paperless-gpt for more than a day", Count: stuck,
			Query: "tags__id__in=" + tidyJoinInts(queue) + "&added__date__lt=" + queueCut.Format("2006-01-02")})
	}
	if len(fail) > 0 {
		items = append(items, tidyHygieneItem{Key: "failed", Label: "Marked failed by paperless-gpt", Count: failed,
			Query: fmt.Sprintf("tags__id__all=%d", fail[0])})
	}
	return items
}

// --- custom-field value audit ---

type tidyValueCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type tidyFieldAudit struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	DataType string `json:"data_type"`
	// Documents that carry the field, and how many of those leave it empty.
	DocCount   int `json:"doc_count"`
	EmptyCount int `json:"empty_count"`
	Distinct   int `json:"distinct"`
	// The most common values, at most tidyAuditTop.
	Top []tidyValueCount `json:"top"`
	// Values that differ only in case, spacing or punctuation.
	Variants [][]tidyValueCount `json:"variants"`
	// Select options no document uses.
	UnusedOptions []string `json:"unused_options"`
	// Monetary values written without a currency code.
	NoCurrency int `json:"no_currency,omitempty"`
}

const tidyAuditTop = 25

// tidyValueKey folds a value for the variant check: lowercase, letters and
// digits only. "2025, 2026" and "2025,2026" share a key.
func tidyValueKey(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tidyFieldDisplay renders a raw custom-field value as text, mapping select
// option ids to labels. ok is false for an empty value.
func tidyFieldDisplay(raw json.RawMessage, options map[string]string) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw), true
	}
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return "", false
		}
		if label, ok := options[x]; ok {
			return label, true
		}
		return x, true
	case bool:
		return fmt.Sprint(x), true
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", x), "0"), "."), true
	case []any:
		if len(x) == 0 {
			return "", false
		}
		return fmt.Sprintf("%d linked", len(x)), true
	}
	return string(raw), true
}

func tidyBuildFieldAudit(docs []tidyDoc, fields []tidyObject) []tidyFieldAudit {
	type extra struct {
		SelectOptions []SelectOption `json:"select_options"`
	}
	out := make([]tidyFieldAudit, 0, len(fields))
	for _, f := range fields {
		var raw struct {
			ExtraData *extra `json:"extra_data"`
		}
		_ = json.Unmarshal(f.Raw, &raw)
		options := map[string]string{}
		var optionOrder []string
		if raw.ExtraData != nil {
			for _, o := range raw.ExtraData.SelectOptions {
				options[o.ID] = o.Label
				optionOrder = append(optionOrder, o.Label)
			}
		}
		a := tidyFieldAudit{ID: f.ID, Name: f.Name, DataType: f.DataType, Top: []tidyValueCount{},
			Variants: [][]tidyValueCount{}, UnusedOptions: []string{}}
		counts := map[string]int{}
		for _, d := range docs {
			for _, cf := range d.CustomFields {
				if cf.Field != f.ID {
					continue
				}
				a.DocCount++
				text, ok := tidyFieldDisplay(cf.Value, options)
				if !ok {
					a.EmptyCount++
					continue
				}
				if f.DataType == "documentlink" {
					text = "(links)"
				}
				counts[text]++
				if f.DataType == "monetary" {
					if s := strings.TrimSpace(text); s != "" && (s[0] >= '0' && s[0] <= '9' || s[0] == '-' || s[0] == '.') {
						a.NoCurrency++
					}
				}
			}
		}
		a.Distinct = len(counts)
		all := make([]tidyValueCount, 0, len(counts))
		for v, n := range counts {
			all = append(all, tidyValueCount{Value: v, Count: n})
		}
		sort.Slice(all, func(i, j int) bool {
			if all[i].Count != all[j].Count {
				return all[i].Count > all[j].Count
			}
			return all[i].Value < all[j].Value
		})
		if len(all) > tidyAuditTop {
			a.Top = all[:tidyAuditTop]
		} else {
			a.Top = all
		}
		if f.DataType == "string" || f.DataType == "select" || f.DataType == "url" {
			byKey := map[string][]tidyValueCount{}
			for _, vc := range all {
				k := tidyValueKey(vc.Value)
				if k != "" {
					byKey[k] = append(byKey[k], vc)
				}
			}
			for _, group := range byKey {
				if len(group) > 1 {
					a.Variants = append(a.Variants, group)
				}
			}
			sort.Slice(a.Variants, func(i, j int) bool { return a.Variants[i][0].Value < a.Variants[j][0].Value })
		}
		for _, label := range optionOrder {
			if counts[label] == 0 {
				a.UnusedOptions = append(a.UnusedOptions, label)
			}
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}
