package main

import (
	"context"
	"fmt"
	"paperless-gpt/ocr"
	"strings"

	"github.com/tmc/langchaingo/llms"
	"google.golang.org/genai"
)

// GoogleAIProvider implements the LLMProvider interface for Google Gemini API using google.golang.org/genai
type GoogleAIProvider struct {
	client         *genai.Client
	thinkingBudget *int32
	thinkingLevel  string
	model          string
}

// NewGoogleAIProvider creates a new GoogleAIProvider instance. thinkingLevel
// (Gemini 3 and later, e.g. "low") takes precedence over thinkingBudget, since
// the API rejects a request that sets both.
func NewGoogleAIProvider(ctx context.Context, model string, apiKey string, thinkingBudget *int32, thinkingLevel string) (*GoogleAIProvider, error) {
	client, err := ocr.NewGenAIClient(ctx, apiKey)
	if err != nil {
		return nil, err
	}

	return &GoogleAIProvider{
		client:         client,
		thinkingBudget: thinkingBudget,
		thinkingLevel:  thinkingLevel,
		model:          model,
	}, nil
}

// thinkingConfig returns the thinking settings for a request, or nil to use
// the model's default.
func (p *GoogleAIProvider) thinkingConfig() *genai.ThinkingConfig {
	if p.thinkingLevel != "" {
		return &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevel(strings.ToUpper(p.thinkingLevel))}
	}
	if p.thinkingBudget != nil {
		return &genai.ThinkingConfig{ThinkingBudget: genai.Ptr(*p.thinkingBudget)}
	}
	return nil
}

// GenerateText sends a text generation request to Gemini API
func (p *GoogleAIProvider) GenerateText(ctx context.Context, prompt string) (string, error) {
	if p.client == nil {
		return "", fmt.Errorf("googleai client not initialized")
	}

	// Prepare generation config with thinking settings if set
	var genConfig *genai.GenerateContentConfig
	if tc := p.thinkingConfig(); tc != nil {
		genConfig = &genai.GenerateContentConfig{ThinkingConfig: tc}
	}

	contents := genai.Text(prompt)

	resp, err := p.client.Models.GenerateContent(ctx, p.model, contents, genConfig)
	if err != nil {
		return "", fmt.Errorf("googleai GenerateContent API error: %w", err)
	}

	if resp == nil || len(resp.Candidates) == 0 {
		return "", fmt.Errorf("googleai GenerateContent API returned empty response")
	}
	if u := resp.UsageMetadata; u != nil {
		log.Debugf("googleai usage: prompt=%d output=%d thinking=%d", u.PromptTokenCount, u.CandidatesTokenCount, u.ThoughtsTokenCount)
	}

	candidate := resp.Candidates[0]
	if candidate.Content == nil {
		return "", fmt.Errorf("googleai GenerateContent API returned a candidate with nil content")
	}

	if candidate.Content.Parts == nil || len(candidate.Content.Parts) == 0 {
		return "", fmt.Errorf("googleai GenerateContent API returned a candidate with no content parts")
	}

	if candidate.Content.Parts[0].Text == "" {
		return "", fmt.Errorf("googleai GenerateContent API returned a candidate with empty text")
	}

	return candidate.Content.Parts[0].Text, nil
}

// Close closes any resources held by the provider
func (p *GoogleAIProvider) Close() error {
	// The genai.Client does not have a Close method, so nothing to do here
	return nil
}

/*
GenerateContent implements the llms.Model interface for GoogleAIProvider.
It adapts a single-message prompt to the Google Gemini API and wraps the result.
*/
func (p *GoogleAIProvider) GenerateContent(ctx context.Context, messages []llms.MessageContent, opts ...llms.CallOption) (*llms.ContentResponse, error) {
	if len(messages) == 0 || len(messages[0].Parts) == 0 {
		return nil, fmt.Errorf("no prompt provided")
	}
	textPart, ok := messages[0].Parts[0].(llms.TextContent)
	if !ok {
		return nil, fmt.Errorf("first message part is not TextContent")
	}
	result, err := p.GenerateText(ctx, textPart.Text)
	if err != nil {
		return nil, err
	}
	return &llms.ContentResponse{
		Choices: []*llms.ContentChoice{
			{
				Content: result,
			},
		},
	}, nil
}

// Call implements the llms.Model interface for compatibility with langchaingo.
// It takes a plain string prompt and returns the generated text.
func (p *GoogleAIProvider) Call(ctx context.Context, prompt string, opts ...llms.CallOption) (string, error) {
	return p.GenerateText(ctx, prompt)
}

// IsGoogleAIProvider returns true for this provider
func (p *GoogleAIProvider) IsGoogleAIProvider() bool {
	return true
}

// ProviderName returns the provider name
func (p *GoogleAIProvider) ProviderName() string {
	return "googleai"
}
