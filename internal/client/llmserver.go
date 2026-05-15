package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// LLMServerClient is the host-side LLM backend used by ProcessClient to
// fulfill `llm.chat` requests issued by the banya-core sidecar. It speaks
// the OpenAI-compatible chat-completions protocol exposed by llm-server
// (LLM Lab Client Manager :8083 / public proxy :5174). It is NOT a
// top-level Client mode — the cli always goes through banya-core
// (sidecar or remote).
type LLMServerClient struct {
	baseURL    string
	apiKey     string
	model      string
	targetPort string // X-Target-Port header, routes to a specific vLLM instance
	http       *http.Client

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// Defaults for the LLM Lab endpoint (5174 is the public proxy that
// forwards to the Client Manager on :8083 behind the firewall).
const (
	DefaultLLMServerURL        = "http://118.37.145.31:5174"
	DefaultLLMServerAPIKey     = "sk-959b0eb4a8899f7e194f294eeebde0235956425ba77c56de"
	DefaultLLMServerModel      = "/models/model"
	DefaultLLMServerTargetPort = "8085" // Qwen3.5-122B-A10B vLLM instance
)

// openaiMessage is the OpenAI chat-completions message format.
type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiChatRequest struct {
	Model       string             `json:"model"`
	Messages    []openaiMessage    `json:"messages"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	TopP        float64            `json:"top_p,omitempty"`
	Stream      bool               `json:"stream"`
	Tools       []openaiTool       `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`
}

type openaiTool struct {
	Type     string                 `json:"type"`
	Function openaiToolSpecFunction `json:"function"`
}

type openaiToolSpecFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type openaiStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type openaiStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string                 `json:"content"`
			ToolCalls []openaiStreamToolCall `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// NewLLMServerClient returns a new llm-server backend.
// Empty arguments fall back to package defaults.
func NewLLMServerClient(baseURL, apiKey, model string) *LLMServerClient {
	return NewLLMServerClientWithTarget(baseURL, apiKey, model, "")
}

// NewLLMServerClientWithTarget is NewLLMServerClient with an explicit
// X-Target-Port header. Empty targetPort uses the package default.
func NewLLMServerClientWithTarget(baseURL, apiKey, model, targetPort string) *LLMServerClient {
	if baseURL == "" {
		baseURL = DefaultLLMServerURL
	}
	if apiKey == "" {
		apiKey = DefaultLLMServerAPIKey
	}
	if model == "" {
		model = DefaultLLMServerModel
	}
	if targetPort == "" {
		targetPort = DefaultLLMServerTargetPort
	}
	return &LLMServerClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      model,
		targetPort: targetPort,
		http:       &http.Client{Timeout: 0},
		cancels:    make(map[string]context.CancelFunc),
	}
}

// Chat implements LLMBackend. It runs a single chat completion against
// llm-server, streaming content tokens through onToken. When
// params.Tools is non-empty the model's native tool_calls are
// accumulated per-index across the stream and returned alongside the
// content.
func (c *LLMServerClient) Chat(ctx context.Context, params protocol.LlmChatParams, onToken func(string) error) (string, string, []protocol.LlmToolCall, error) {
	traceStart := time.Now()
	messages := make([]openaiMessage, 0, len(params.Messages))
	for _, m := range params.Messages {
		messages = append(messages, openaiMessage{Role: string(m.Role), Content: m.Content})
	}

	// The client-configured model (from --llm-model flag or config.yaml)
	// takes precedence: banya-core sometimes sends an internal alias like
	// "host" which the upstream llm-server does not recognise. Falling back
	// to params.Model only when no client model is set preserves legacy
	// behaviour for tests that explicitly pass a model through params.
	model := c.model
	if model == "" {
		model = params.Model
	}

	// SWE-bench preset (BANYA_SWE_BENCH=1) overrides sampling for
	// deterministic-leaning output. Code/diff generation is much more
	// reliable at low temperature; lower top_p further trims long-tail
	// drift into analysis prose. Caller-provided params are ignored when
	// the env flag is set so harness-side settings stay consistent across
	// the run.
	temperature := pickFloat(params.Temperature, 0.7)
	topP := pickFloat(params.TopP, 0.95)
	if os.Getenv("BANYA_SWE_BENCH") == "1" {
		temperature = 0.1
		topP = 0.1
	}
	// BO@N override (Strategy b+): when banya-cli's runBoN sets
	// BANYA_SWE_BO_TEMPERATURE / BANYA_SWE_BO_TOP_P per sample, those win
	// over the SWE_BENCH=1 deterministic preset so each of the N samples
	// gets its own point in the temperature/top_p grid for diversity. Env
	// var presence is the signal — empty string means "not in BO@N", fall
	// back to whatever was set above.
	if v := os.Getenv("BANYA_SWE_BO_TEMPERATURE"); v != "" {
		if t, err := strconv.ParseFloat(v, 64); err == nil && t > 0 {
			temperature = t
		}
	}
	if v := os.Getenv("BANYA_SWE_BO_TOP_P"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil && p > 0 {
			topP = p
		}
	}
	// vLLM's `qwen3_coder` tool-call-parser fails to convert the model's
	// native XML format (`<function=name><parameter=k>v</parameter></function>`)
	// to native `tool_calls` when streaming under realistic SWE-bench loads
	// (32 tools + 7K-char system prompt). Empirically the same payload
	// non-streamed parses correctly. `BANYA_LLM_NO_STREAM=1` forces a single
	// POST/JSON response so the parser sees the full output at once.
	useNonStream := os.Getenv("BANYA_LLM_NO_STREAM") == "1"
	reqBody := openaiChatRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   pickInt(params.MaxTokens, 2048),
		Temperature: temperature,
		TopP:        topP,
		Stream:      !useNonStream,
	}
	if len(params.Tools) > 0 {
		reqBody.Tools = make([]openaiTool, 0, len(params.Tools))
		for _, t := range params.Tools {
			reqBody.Tools = append(reqBody.Tools, openaiTool{
				Type: t.Type,
				Function: openaiToolSpecFunction{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				},
			})
		}
		// Mirror banya-eval's sidecar harness: when tools are present but
		// the caller didn't set tool_choice, force "auto". vLLM with
		// --enable-auto-tool-choice otherwise falls back to text-only
		// output for this model, so the agent never actually invokes
		// CREATE_FILE / RUN_COMMAND even when the prompt demands it.
		if params.ToolChoice != nil {
			reqBody.ToolChoice = params.ToolChoice
		} else {
			reqBody.ToolChoice = "auto"
		}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if !useNonStream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.targetPort != "" {
		req.Header.Set("X-Target-Port", c.targetPort)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return "", "", nil, fmt.Errorf("llm-server %d: %s", resp.StatusCode, string(errBody))
	}

	// Non-streaming branch: single-shot JSON response. Used when vLLM's
	// streaming tool-call parser is buggy for the active model (e.g.
	// qwen3_coder).
	if useNonStream {
		bodyBytes, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return "", "", nil, fmt.Errorf("read non-stream body: %w", rerr)
		}
		var nsResp struct {
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
				Message      struct {
					Content   string                 `json:"content"`
					ToolCalls []openaiStreamToolCall `json:"tool_calls,omitempty"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(bodyBytes, &nsResp); err != nil {
			return "", "", nil, fmt.Errorf("parse non-stream response: %w", err)
		}
		if len(nsResp.Choices) == 0 {
			snippet := string(bodyBytes)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			return "", "", nil, fmt.Errorf("non-stream response: no choices in body=%s", snippet)
		}
		ch := nsResp.Choices[0]
		finish := ""
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
		var toolCalls []protocol.LlmToolCall
		for _, tc := range ch.Message.ToolCalls {
			toolCalls = append(toolCalls, protocol.LlmToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		// Fallback: vLLM's qwen3_coder tool-call-parser sometimes returns
		// the model's raw XML in `content` with `tool_calls` empty under
		// large-payload scenarios. Recover by parsing the XML here so
		// banya-core sees the tool_calls regardless of vLLM behaviour.
		if len(toolCalls) == 0 && ch.Message.Content != "" {
			recovered := parseQwenXMLToolCalls(ch.Message.Content)
			if len(recovered) > 0 {
				toolCalls = recovered
				// Strip the parsed XML from content so banya-core's
				// fallback text parser doesn't double-count.
				ch.Message.Content = ""
				if finish == "stop" || finish == "" {
					finish = "tool_calls"
				}
			}
		}
		// Mirror streaming behaviour: pipe content through onToken once.
		if onToken != nil && ch.Message.Content != "" {
			if cbErr := onToken(ch.Message.Content); cbErr != nil {
				return ch.Message.Content, finish, toolCalls, cbErr
			}
		}
		if tracePath := os.Getenv("BANYA_LLM_TRACE_PATH"); tracePath != "" {
			writeLLMServerTrace(tracePath, model, params, reqBody, ch.Message.Content, finish, toolCalls, time.Since(traceStart))
		}
		return ch.Message.Content, finish, toolCalls, nil
	}

	var content strings.Builder
	var finish string
	toolAcc := map[int]*protocol.LlmToolCall{}

	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return content.String(), finish, collectToolCalls(toolAcc), fmt.Errorf("read stream: %w", rerr)
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
					return content.String(), finish, collectToolCalls(toolAcc), cbErr
				}
			}
		}
		for _, delta := range chunk.Choices[0].Delta.ToolCalls {
			entry := toolAcc[delta.Index]
			if entry == nil {
				entry = &protocol.LlmToolCall{}
				toolAcc[delta.Index] = entry
			}
			if delta.ID != "" {
				entry.ID = delta.ID
			}
			if delta.Function.Name != "" {
				entry.Name = delta.Function.Name
			}
			if delta.Function.Arguments != "" {
				entry.Arguments += delta.Function.Arguments
			}
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil {
			finish = *fr
			break
		}
	}
	finalContent := content.String()
	finalTools := collectToolCalls(toolAcc)
	if tracePath := os.Getenv("BANYA_LLM_TRACE_PATH"); tracePath != "" {
		writeLLMServerTrace(tracePath, model, params, reqBody, finalContent, finish, finalTools, time.Since(traceStart))
	}
	return finalContent, finish, finalTools, nil
}

// writeLLMServerTrace dumps a raw request/response record for diagnostic
// inspection. Captures the EXACT messages + tools + tool_choice we sent
// plus the model's content/finish/tool_calls. Best-effort; never errors.
func writeLLMServerTrace(
	path, model string,
	params protocol.LlmChatParams,
	reqBody openaiChatRequest,
	completion, finish string,
	toolCalls []protocol.LlmToolCall,
	elapsed time.Duration,
) {
	rec := map[string]any{
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
		"provider":   "llm-server",
		"model":      model,
		"elapsed_ms": elapsed.Milliseconds(),
		"workspace":  currentWorkspace(),
		"request": map[string]any{
			"messages":     reqBody.Messages,
			"tools":        reqBody.Tools,
			"tool_choice":  reqBody.ToolChoice,
			"max_tokens":   reqBody.MaxTokens,
			"temperature":  reqBody.Temperature,
			"top_p":        reqBody.TopP,
			"params_tools": params.Tools,
		},
		"response": map[string]any{
			"content":    completion,
			"finish":     finish,
			"tool_calls": toolCalls,
		},
	}
	target := resolveTracePath(path)
	if target == "" {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	traceWriteMu.Lock()
	defer traceWriteMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
	_, _ = f.Write([]byte{'\n'})
}

// collectToolCalls emits the accumulated tool_call map in stable index
// order, dropping entries that never received a name (partial deltas).
func collectToolCalls(acc map[int]*protocol.LlmToolCall) []protocol.LlmToolCall {
	if len(acc) == 0 {
		return nil
	}
	maxIdx := -1
	for idx := range acc {
		if idx > maxIdx {
			maxIdx = idx
		}
	}
	out := make([]protocol.LlmToolCall, 0, len(acc))
	for i := 0; i <= maxIdx; i++ {
		if tc, ok := acc[i]; ok && tc.Name != "" {
			out = append(out, *tc)
		}
	}
	return out
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
	if c.targetPort != "" {
		req.Header.Set("X-Target-Port", c.targetPort)
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
