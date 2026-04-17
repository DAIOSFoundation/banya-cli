package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// LLMServerClient is the host-side LLM backend used by ProcessClient to
// fulfill `llm.chat` requests issued by the banya-core sidecar. It speaks
// the OpenAI-compatible chat-completions protocol exposed by llm-server
// (LLM Lab Client Manager :8083). It is NOT a top-level Client mode —
// the cli always goes through banya-core (sidecar or remote).
type LLMServerClient struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// Defaults for the LLM Lab endpoint (5174 is the public proxy that
// forwards to the Client Manager on :8083 behind the firewall).
const (
	DefaultLLMServerURL    = "http://118.37.145.31:5174"
	DefaultLLMServerAPIKey = "sk-959b0eb4a8899f7e194f294eeebde0235956425ba77c56de"
	DefaultLLMServerModel  = "/models/model"
)

// openaiMessage is the OpenAI chat-completions message format.
type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openaiMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	Stream      bool            `json:"stream"`
}

type openaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// NewLLMServerClient returns a new llm-server backend.
// Empty arguments fall back to package defaults.
func NewLLMServerClient(baseURL, apiKey, model string) *LLMServerClient {
	if baseURL == "" {
		baseURL = DefaultLLMServerURL
	}
	if apiKey == "" {
		apiKey = DefaultLLMServerAPIKey
	}
	if model == "" {
		model = DefaultLLMServerModel
	}
	return &LLMServerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Timeout: 0},
		cancels: make(map[string]context.CancelFunc),
	}
}

// Chat implements LLMBackend. It runs a single chat completion against
// llm-server, streaming tokens through onToken until the model finishes.
func (c *LLMServerClient) Chat(ctx context.Context, params protocol.LlmChatParams, onToken func(string) error) (string, string, error) {
	messages := make([]openaiMessage, 0, len(params.Messages))
	for _, m := range params.Messages {
		messages = append(messages, openaiMessage{Role: string(m.Role), Content: m.Content})
	}

	model := params.Model
	if model == "" {
		model = c.model
	}

	body, err := json.Marshal(openaiChatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   pickInt(params.MaxTokens, 2048),
		Temperature: pickFloat(params.Temperature, 0.7),
		TopP:        pickFloat(params.TopP, 0.95),
		Stream:      true,
	})
	if err != nil {
		return "", "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("llm-server %d: %s", resp.StatusCode, string(errBody))
	}

	var content strings.Builder
	var finish string
	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return content.String(), finish, fmt.Errorf("read stream: %w", rerr)
		}
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk openaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if tok := chunk.Choices[0].Delta.Content; tok != "" {
			content.WriteString(tok)
			if onToken != nil {
				if cbErr := onToken(tok); cbErr != nil {
					return content.String(), finish, cbErr
				}
			}
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			finish = *fr
			break
		}
	}
	return content.String(), finish, nil
}

// HealthCheck pings llm-server with a minimal probe.
func (c *LLMServerClient) HealthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	probe, _ := json.Marshal(openaiChatRequest{
		Model:     c.model,
		Messages:  []openaiMessage{{Role: "user", Content: "ping"}},
		MaxTokens: 1,
		Stream:    false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(probe))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("llm-server unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("llm-server unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// Close cancels any in-flight streams.
func (c *LLMServerClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.cancels {
		cancel()
	}
	c.cancels = map[string]context.CancelFunc{}
	return nil
}

func pickInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func pickFloat(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

var _ LLMBackend = (*LLMServerClient)(nil)
