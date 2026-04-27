// GeminiCriticProvider — wraps the original REST call path so the rest
// of the package can treat Gemini as just another CriticProvider.
// Behaviour is identical to the pre-refactor inline implementation;
// the code here only moves bytes between critic.go and the HTTP layer.

package critic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// criticMaxOutputTokens — gemini-2.5-pro is thinking-mode-only and
// thinking tokens share the maxOutputTokens budget with the visible
// response. 4K caused complex patch reviews to drain the budget on
// thinking and return finishReason=MAX_TOKENS with no candidate parts,
// surfacing as "(critic empty response)" in critic.go. 32K leaves
// room for both thinking and a full GapObject JSON payload.
const criticMaxOutputTokens = 32768

type GeminiProvider struct {
	APIKey   string
	Model    string
	Endpoint string
	Timeout  time.Duration
	client   *http.Client
}

func NewGeminiProvider(apiKey, model, endpoint string, timeout time.Duration) *GeminiProvider {
	if model == "" {
		model = DefaultModel
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}
	return &GeminiProvider{
		APIKey: apiKey, Model: model, Endpoint: endpoint, Timeout: timeout,
		client: &http.Client{Timeout: timeout},
	}
}

func (p *GeminiProvider) Name() string { return "gemini" }

func (p *GeminiProvider) Review(ctx context.Context, args ReviewArgs) (string, error) {
	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": args.ReviewPrompt}}},
		},
		"generationConfig": map[string]any{
			"temperature":      0.2,
			"maxOutputTokens":  criticMaxOutputTokens,
			"responseMimeType": "application/json",
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	url := fmt.Sprintf("%s/%s:generateContent?key=%s", p.Endpoint, p.Model, p.APIKey)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini call: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("gemini http %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var apiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		PromptFeedback json.RawMessage `json:"promptFeedback"`
	}
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		// Empty parts can happen for several reasons — finishReason and
		// the thinking-vs-candidates token split are the load-bearing
		// signals to distinguish them. Surface them on stderr so the
		// sidecar log captures the cause instead of silently REVISE-ing.
		fr := ""
		if len(apiResp.Candidates) > 0 {
			fr = apiResp.Candidates[0].FinishReason
		}
		um := apiResp.UsageMetadata
		fmt.Fprintf(os.Stderr,
			"[critic/gemini] empty response: finishReason=%q usage{prompt=%d candidates=%d thoughts=%d total=%d} promptFeedback=%s\n",
			fr, um.PromptTokenCount, um.CandidatesTokenCount, um.ThoughtsTokenCount, um.TotalTokenCount,
			truncate(string(apiResp.PromptFeedback), 200))
		return "", nil
	}
	return apiResp.Candidates[0].Content.Parts[0].Text, nil
}
