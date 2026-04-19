// GeminiBackend — an LLMBackend implementation that speaks directly to
// Google's Generative Language REST API (Gemini 3 Flash Preview, Gemini
// 2.5 Pro, …). Used when the cli is invoked with --llm-backend=gemini.
//
// Why exist alongside LLMServerClient?
//   On-prem vLLM (Qwen3.5-122B-A10B) is slow on the hardware this repo
//   ships with. SIBDD and paper ablations need 10×+ iteration speed;
//   Gemini Flash over API delivers that. Gemini also exposes native
//   function-calling (declarations with responseMimeType), which lets
//   us keep the same native-tool path banya-core uses for Anthropic
//   and OpenAI-compat.
//
// Streaming: Gemini's `streamGenerateContent?alt=sse` route produces
// SSE chunks shaped like standard `data: {...}` lines wrapping
// `candidates[].content.parts[]`. We treat text parts as tokens (fed
// to onToken), functionCall parts as native tool calls (accumulated
// and returned once the stream terminates).
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
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// Defaults chosen so dev-mode ("Gemini Flash main agent") needs no
// plumbing other than an API key.
const (
	DefaultGeminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models"
	DefaultGeminiModel    = "gemini-3-flash-preview"
)

type GeminiBackend struct {
	endpoint string
	apiKey   string
	model    string
	http     *http.Client
}

// NewGeminiBackend returns a backend. Empty arguments fall back to
// defaults; the api key MUST be supplied (via flag or env) for Chat to
// succeed — Gemini REST has no anonymous tier.
func NewGeminiBackend(endpoint, apiKey, model string) *GeminiBackend {
	if endpoint == "" {
		endpoint = DefaultGeminiEndpoint
	}
	if model == "" {
		model = DefaultGeminiModel
	}
	return &GeminiBackend{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
		http:     &http.Client{Timeout: 0}, // callers supply ctx timeout
	}
}

// Gemini request/response shapes. Kept close to the REST docs so a
// reviewer can cross-check the mapping in one pass. Only the fields we
// use are declared — unknown fields are ignored by json.Decoder.

type gemPart struct {
	Text         string             `json:"text,omitempty"`
	FunctionCall *gemFunctionCall   `json:"functionCall,omitempty"`
	Thought      bool               `json:"thought,omitempty"`
}

type gemFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type gemContent struct {
	Role  string    `json:"role,omitempty"`
	Parts []gemPart `json:"parts"`
}

type gemCandidate struct {
	Content      gemContent `json:"content"`
	FinishReason string     `json:"finishReason,omitempty"`
}

type gemStreamChunk struct {
	Candidates []gemCandidate `json:"candidates,omitempty"`
}

type gemToolFunctionDecl struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]any         `json:"parameters,omitempty"`
}

type gemTool struct {
	FunctionDeclarations []gemToolFunctionDecl `json:"functionDeclarations"`
}

type gemSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type gemRequest struct {
	Contents          []gemContent       `json:"contents"`
	SystemInstruction *gemContent        `json:"systemInstruction,omitempty"`
	GenerationConfig  map[string]any     `json:"generationConfig,omitempty"`
	SafetySettings    []gemSafetySetting `json:"safetySettings,omitempty"`
	Tools             []gemTool          `json:"tools,omitempty"`
	ToolConfig        map[string]any     `json:"toolConfig,omitempty"`
}

// Safety settings: open up as far as Gemini REST allows. Agent tasks
// legitimately need to discuss patch-the-exploit, security bugs, etc.,
// and we already have our own safety rules in the system prompt.
var geminiSafetyOpen = []gemSafetySetting{
	{"HARM_CATEGORY_HARASSMENT", "BLOCK_NONE"},
	{"HARM_CATEGORY_HATE_SPEECH", "BLOCK_NONE"},
	{"HARM_CATEGORY_SEXUALLY_EXPLICIT", "BLOCK_NONE"},
	{"HARM_CATEGORY_DANGEROUS_CONTENT", "BLOCK_NONE"},
}

// Chat implements LLMBackend.
//
// We always use streamGenerateContent so onToken gets real-time tokens;
// callers that don't care about streaming simply ignore the callback
// (we still need the SSE path to capture function calls incrementally).
func (g *GeminiBackend) Chat(
	ctx context.Context,
	params protocol.LlmChatParams,
	onToken func(string) error,
) (string, string, []protocol.LlmToolCall, error) {
	if g.apiKey == "" {
		return "", "", nil, fmt.Errorf("gemini backend: no api key (set BANYA_MAIN_API_KEY or pass --llm-key)")
	}

	// Split system messages out — Gemini expects them in
	// systemInstruction, not Contents.
	var sysText strings.Builder
	contents := make([]gemContent, 0, len(params.Messages))
	for _, m := range params.Messages {
		role := strings.ToLower(string(m.Role))
		if role == "system" {
			if sysText.Len() > 0 {
				sysText.WriteString("\n\n")
			}
			sysText.WriteString(m.Content)
			continue
		}
		// Gemini uses "user" / "model" (not "assistant"). Map.
		if role == "assistant" {
			role = "model"
		}
		contents = append(contents, gemContent{
			Role:  role,
			Parts: []gemPart{{Text: m.Content}},
		})
	}
	// Gemini requires non-empty contents; synthesize a user turn from
	// system text when callers forgot (rare but guard).
	if len(contents) == 0 && sysText.Len() > 0 {
		contents = append(contents, gemContent{
			Role:  "user",
			Parts: []gemPart{{Text: sysText.String()}},
		})
		sysText.Reset()
	}

	modelToUse := g.model
	if params.Model != "" {
		// Allow per-call override, but only when caller is explicit — this
		// mirrors llmserver.go's "client-configured model wins".
		modelToUse = params.Model
	}

	body := gemRequest{
		Contents:       contents,
		SafetySettings: geminiSafetyOpen,
		GenerationConfig: map[string]any{
			"temperature":     pickFloatGem(params.Temperature, 0.7),
			"topP":            pickFloatGem(params.TopP, 0.95),
			"maxOutputTokens": pickIntGem(params.MaxTokens, 4096),
		},
	}
	// Thinking configuration. Gemini 2.x defaults to unlimited thinking
	// (thinkingBudget: -1), which we've observed STARVES tool calls:
	// the model "thinks" an answer into text instead of emitting
	// functionCall. Gemini 3.x uses a different schema (thinkingLevel
	// enum). We mirror banya-core's GeminiProvider.ts choices:
	//   gemini-3*     → thinkingLevel: "medium"
	//   gemini-2.5*   → thinkingBudget: -1 (kept for parity; if tool
	//                   use is still starved, caller can override via
	//                   BANYA_MAIN_MODEL=gemini-2.5-flash which has
	//                   smaller default thinking budget).
	// When tools are present we set `includeThoughts: false` so the
	// SSE stream never has to split thought/answer parts.
	isGemini3 := strings.Contains(modelToUse, "gemini-3")
	thinkingCfg := map[string]any{}
	if isGemini3 {
		thinkingCfg["thinkingLevel"] = "medium"
	} else {
		thinkingCfg["thinkingBudget"] = -1
	}
	thinkingCfg["includeThoughts"] = false
	body.GenerationConfig["thinkingConfig"] = thinkingCfg

	if sysText.Len() > 0 {
		body.SystemInstruction = &gemContent{Parts: []gemPart{{Text: sysText.String()}}}
	}
	if len(params.Tools) > 0 {
		decls := make([]gemToolFunctionDecl, 0, len(params.Tools))
		for _, t := range params.Tools {
			// Gemini expects an OpenAPI-3-subset schema with UPPERCASE
			// types (OBJECT / STRING / ARRAY / NUMBER / BOOLEAN / INTEGER).
			// banya-core ships OpenAI-compat lowercase schemas; normalise
			// recursively before sending or Gemini silently ignores tools.
			params := normaliseGeminiSchema(t.Function.Parameters)
			decls = append(decls, gemToolFunctionDecl{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  params,
			})
		}
		body.Tools = []gemTool{{FunctionDeclarations: decls}}
		// "AUTO" lets the model pick text vs tool; "ANY" forces a tool
		// call every turn. Agent loops expect final text messages too
		// (when the task is done), so "AUTO" is the right default.
		// If tools are persistently starved we can expose an env flag
		// BANYA_GEMINI_TOOL_MODE to override, but that's an escape hatch
		// not a primary knob.
		mode := "AUTO"
		if envMode := getEnv("BANYA_GEMINI_TOOL_MODE"); envMode != "" {
			mode = envMode
		}
		body.ToolConfig = map[string]any{
			"function_calling_config": map[string]any{"mode": mode},
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", "", nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf(
		"%s/%s:streamGenerateContent?alt=sse&key=%s",
		g.endpoint, modelToUse, g.apiKey,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", "", nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", "", nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return "", "", nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, truncateGem(string(errBody), 400))
	}

	var content strings.Builder
	var finish string
	// Gemini emits fully-formed functionCalls per chunk (no delta
	// accumulation like OpenAI). We append as they arrive.
	var toolCalls []protocol.LlmToolCall

	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return content.String(), finish, toolCalls, fmt.Errorf("read stream: %w", rerr)
		}
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk gemStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Candidates) == 0 {
			continue
		}
		cand := chunk.Candidates[0]
		if cand.FinishReason != "" {
			finish = cand.FinishReason
		}
		for _, p := range cand.Content.Parts {
			// Skip "thought" parts from onToken — callers don't want
			// chain-of-thought text mingled with the answer.
			if p.Thought {
				continue
			}
			if p.Text != "" {
				content.WriteString(p.Text)
				if onToken != nil {
					if cbErr := onToken(p.Text); cbErr != nil {
						return content.String(), finish, toolCalls, cbErr
					}
				}
			}
			if p.FunctionCall != nil {
				tc := protocol.LlmToolCall{
					Name:      p.FunctionCall.Name,
					Arguments: string(p.FunctionCall.Args),
				}
				toolCalls = append(toolCalls, tc)
			}
		}
	}
	return content.String(), finish, toolCalls, nil
}

func (g *GeminiBackend) Close() error { return nil }

// normaliseGeminiSchema converts an OpenAI-compat JSON schema fragment
// (type: "object"/"string"/...) into the Gemini OpenAPI-3 subset
// (type: "OBJECT"/"STRING"/...). Recurses into `properties` + `items`.
// Unknown keys pass through untouched so custom annotations survive.
func normaliseGeminiSchema(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "type" {
			if s, ok := v.(string); ok {
				out[k] = strings.ToUpper(s)
				continue
			}
		}
		switch vv := v.(type) {
		case map[string]any:
			out[k] = normaliseGeminiSchema(vv)
		case []any:
			out[k] = normaliseGeminiSchemaSlice(vv)
		default:
			out[k] = v
		}
	}
	return out
}

func normaliseGeminiSchemaSlice(items []any) []any {
	out := make([]any, 0, len(items))
	for _, v := range items {
		if m, ok := v.(map[string]any); ok {
			out = append(out, normaliseGeminiSchema(m))
		} else {
			out = append(out, v)
		}
	}
	return out
}

// getEnv is a thin wrapper so the file-level flow reads cleanly.
func getEnv(key string) string { return os.Getenv(key) }

// HealthCheck mirrors LLMServerClient's ping — a one-token generate
// against the configured model. Lets `banya ping` surface mis-configured
// API keys before the first real turn.
func (g *GeminiBackend) HealthCheck() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(gemRequest{
		Contents: []gemContent{{Role: "user", Parts: []gemPart{{Text: "ping"}}}},
		GenerationConfig: map[string]any{
			"temperature":     0,
			"maxOutputTokens": 1,
		},
	})
	url := fmt.Sprintf(
		"%s/%s:generateContent?key=%s",
		g.endpoint, g.model, g.apiKey,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("gemini unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("gemini health %d", resp.StatusCode)
	}
	return nil
}

var _ LLMBackend = (*GeminiBackend)(nil)

// Local clones of the pickInt / pickFloat helpers from llmserver.go —
// defining here too avoids cross-file coupling.
func pickIntGem(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func pickFloatGem(v, def float64) float64 {
	if v > 0 {
		return v
	}
	return def
}

func truncateGem(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
