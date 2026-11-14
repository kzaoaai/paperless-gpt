package main

// Tidy: duplicate groups proposed by the LLM. They are only suggestions: the
// page applies a group only when it is ticked, as ordinary logged merges.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type tidyLLMGroup struct {
	IDs    []int  `json:"ids"`
	Keep   int    `json:"keep"`
	Reason string `json:"reason"`
}

const tidyLLMPrompt = `Below are the %s in a paperless-ngx document archive, one per line as: id | name | number of documents.

Find groups of entries that name the same real-world thing: the same company, agency or person written differently, an abbreviation and its full name, spelling variants, or a misspelling. Do not group things that are only related (two different banks, a company and its subsidiary, a city and its county, a person and their employer, a general category and a specific member of it).

For each group pick the entry to keep: the most complete, correctly spelled name, and on a tie the one with more documents.

Answer with JSON only, no prose, in this shape:
[{"ids": [12, 40], "keep": 40, "reason": "ACME is the abbreviation of Acme Manufacturing Company"}]
Answer [] when there are none.

%s`

var tidyThinkRE = regexp.MustCompile(`(?s)<think>.*?</think>`)

// tidyParseLLMGroups reads the model's answer and keeps only groups of two or
// more known ids whose keep is one of them.
func tidyParseLLMGroups(answer string, known map[int]bool) ([]tidyLLMGroup, error) {
	answer = tidyThinkRE.ReplaceAllString(answer, "")
	start, end := strings.Index(answer, "["), strings.LastIndex(answer, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("the LLM answer holds no JSON list: %.200q", answer)
	}
	var raw []tidyLLMGroup
	if err := json.Unmarshal([]byte(answer[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("the LLM answer is not the expected JSON: %w", err)
	}
	groups := []tidyLLMGroup{}
	for _, g := range raw {
		var ids []int
		for _, id := range tidyUniqueSorted(g.IDs) {
			if known[id] {
				ids = append(ids, id)
			}
		}
		keepOK := false
		for _, id := range ids {
			if id == g.Keep {
				keepOK = true
			}
		}
		if len(ids) < 2 || !keepOK {
			continue
		}
		groups = append(groups, tidyLLMGroup{IDs: ids, Keep: g.Keep, Reason: g.Reason})
	}
	return groups, nil
}

func (s *tidyService) llmGroups(ctx context.Context, kind string) ([]tidyLLMGroup, error) {
	if s.llm == nil {
		return nil, tidyRefuse("no LLM is configured")
	}
	if !tidyMergeable(kind) {
		return nil, tidyRefuse("%s cannot be merged", kind)
	}
	inv, err := s.inventory(ctx, false)
	if err != nil {
		return nil, err
	}
	known := map[int]bool{}
	var lines strings.Builder
	for _, r := range inv.Objects[kind] {
		known[r.ID] = true
		fmt.Fprintf(&lines, "%d | %s | %d\n", r.ID, strings.ReplaceAll(r.Name, "\n", " "), r.DocCount)
	}
	if len(known) < 2 {
		return []tidyLLMGroup{}, nil
	}
	answer, err := s.llm(ctx, fmt.Sprintf(tidyLLMPrompt, strings.ReplaceAll(kind, "_", " "), lines.String()))
	if err != nil {
		return nil, fmt.Errorf("asking the LLM: %w", err)
	}
	return tidyParseLLMGroups(answer, known)
}
