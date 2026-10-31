package main

import (
	"testing"

	"google.golang.org/genai"
)

func TestGoogleAIThinkingConfig(t *testing.T) {
	budget := int32(512)

	if tc := (&GoogleAIProvider{}).thinkingConfig(); tc != nil {
		t.Errorf("no settings: got %+v, want nil (model default)", tc)
	}

	tc := (&GoogleAIProvider{thinkingBudget: &budget}).thinkingConfig()
	if tc == nil || tc.ThinkingBudget == nil || *tc.ThinkingBudget != 512 || tc.ThinkingLevel != "" {
		t.Errorf("budget only: got %+v, want budget 512", tc)
	}

	// The API rejects a request with both, so the level wins.
	tc = (&GoogleAIProvider{thinkingBudget: &budget, thinkingLevel: "low"}).thinkingConfig()
	if tc == nil || tc.ThinkingLevel != genai.ThinkingLevelLow || tc.ThinkingBudget != nil {
		t.Errorf("level and budget: got %+v, want level LOW and no budget", tc)
	}
}
