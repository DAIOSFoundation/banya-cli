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

// Per-attempt HTTP timeout. v8 evidence: gemini-2.5-pro intermittently
// hangs for the full caller-side timeout (90s @ run.go:607). Setting an
// aggressive per-attempt deadline + retries fails fast on a stalled
// connection so a transient API hang doesn't kill a 1200s trajectory.
// Gemini's recommendation in the v8 design review: 45s + 3 retries.
const (
	geminiPerAttemptTimeout = 45 * time.Second
	geminiMaxAttempts       = 3
)

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
	// Inner HTTP client uses per-attempt deadline; outer caller's ctx
	// still bounds total wall time across retries.
	return &GeminiProvider{
		APIKey: apiKey, Model: model, Endpoint: endpoint, Timeout: timeout,
		client: &http.Client{Timeout: geminiPerAttemptTimeout},
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

	// Retry loop: gemini-2.5-pro intermittently hangs/5xx's. We bound
	// EACH attempt with geminiPerAttemptTimeout (45s) and try up to
	// geminiMaxAttempts (3) total. Outer ctx still wins — caller's
	// 90s deadline at run.go:607 covers the full retry window.
	var raw []byte
	var lastErr error
	var statusCode int
	for attempt := 1; attempt <= geminiMaxAttempts; attempt++ {
		// Per-attempt context: min(remaining outer ctx, 45s).
		attemptCtx, cancel := context.WithTimeout(ctx, geminiPerAttemptTimeout)
		req, rerr := http.NewRequestWithContext(attemptCtx, "POST", url, bytes.NewReader(payload))
		if rerr != nil {
			cancel()
			return "", rerr
		}
		req.Header.Set("Content-Type", "application/json")
		resp, derr := p.client.Do(req)
		if derr != nil {
			cancel()
			lastErr = fmt.Errorf("gemini call (attempt %d/%d): %w", attempt, geminiMaxAttempts, derr)
			// Retry on context-deadline / connection errors. The outer
			// caller ctx will terminate the loop if its deadline fires.
			if ctx.Err() != nil {
				return "", lastErr
			}
			backoff := time.Duration(attempt*3) * time.Second
			select {
			case <-time.After(backoff):
				continue
			case <-ctx.Done():
				return "", lastErr
			}
		}
		raw, _ = io.ReadAll(resp.Body)
		statusCode = resp.StatusCode
		resp.Body.Close()
		cancel()

		// Retry only on 5xx + 429. Permanent 4xx returns immediately.
		if statusCode >= 500 || statusCode == 429 {
			lastErr = fmt.Errorf("gemini http %d (attempt %d/%d): %s", statusCode, attempt, geminiMaxAttempts, truncate(string(raw), 200))
			if attempt < geminiMaxAttempts && ctx.Err() == nil {
				backoff := time.Duration(attempt*3) * time.Second
				select {
				case <-time.After(backoff):
					continue
				case <-ctx.Done():
					return "", lastErr
				}
			}
			return "", lastErr
		}
		break // success path
	}
	if raw == nil {
		return "", lastErr
	}
	// 4xx (non-429) falls through to break — surface as terminal error.
	if statusCode >= 400 {
		return "", fmt.Errorf("gemini http %d: %s", statusCode, truncate(string(raw), 400))
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
