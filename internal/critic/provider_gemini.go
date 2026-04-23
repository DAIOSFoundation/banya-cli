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
	"time"
)

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
			// gemini-2.5-pro는 thinking 모드 상시 ON (thinkingBudget=0 거부됨)
			// 이고 thinking 토큰이 maxOutputTokens 한도를 함께 소비한다.
			// 4096으로는 복잡 패치 리뷰 시 thinking 이후 JSON 응답이 잘려
			// `unexpected end of JSON input` 파싱 에러로 이어졌다. 32768로
			// 올려 thinking(수백~수천 토큰) 이후에도 JSON 완결을 보장한다.
			"maxOutputTokens":  32768,
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
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return "", nil
	}
	return apiResp.Candidates[0].Content.Parts[0].Text, nil
}
