// LLM trace logger — shared by every LLMBackend that wants to record
// its (messages, completion) pairs for offline training.
//
// Why a shared file instead of per-backend code? The trace schema is
// benchmark-agnostic: input messages + output completion + metadata
// (model, elapsed, workspace). If a future adapter (GeminiBackend,
// LLMServerClient) wants the same behaviour, it just calls
// appendTrace() — no need to duplicate the file-rotation or JSON
// encoding logic.
//
// Enable by setting BANYA_LLM_TRACE_PATH to a filesystem path. If the
// path is a directory (or ends with /), traces rotate daily into
// <path>/<YYYY-MM-DD>.jsonl. If it's a regular file path, all
// traces append to that single file. Missing parent directories are
// created on demand.
//
// Failure mode: trace logging is best-effort — any I/O error is
// swallowed silently. Never break the benchmark for a log line.
package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// traceWriteMu serialises concurrent appends when multiple goroutines
// share one file. Cheap to hold (one fwrite per turn) and much simpler
// than per-path locking.
var traceWriteMu sync.Mutex

// traceRecord is the on-disk schema. Keep field names stable —
// downstream tooling (HF datasets loader, LoRA training script) reads
// them by name.
type traceRecord struct {
	Timestamp    string                    `json:"timestamp"`
	Provider     string                    `json:"provider"`
	Model        string                    `json:"model"`
	Messages     []protocol.LlmChatMessage `json:"messages"`
	Tools        []protocol.LlmToolSpec    `json:"tools,omitempty"`
	Completion   string                    `json:"completion"`
	ElapsedMs    int64                     `json:"elapsed_ms,omitempty"`
	WorkspaceCwd string                    `json:"workspace,omitempty"`
}

// appendTrace writes one turn to BANYA_LLM_TRACE_PATH. Path semantics:
//   - directory (has trailing / or exists as dir): rotate daily
//   - file path: append-only single file
func appendTrace(
	path string,
	params protocol.LlmChatParams,
	completion string,
	model string,
	elapsed time.Duration,
) {
	record := traceRecord{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Provider:     "claude-cli", // caller is claude-cli today; gemini/llm-server will override this when they adopt the logger
		Model:        model,
		Messages:     params.Messages,
		Tools:        params.Tools,
		Completion:   completion,
		ElapsedMs:    elapsed.Milliseconds(),
		WorkspaceCwd: currentWorkspace(),
	}

	target := resolveTracePath(path)
	if target == "" {
		return
	}

	line, err := json.Marshal(record)
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

// resolveTracePath turns the raw env value into an on-disk file path.
// Directory paths get today's date appended as `<YYYY-MM-DD>.jsonl`.
// File paths are used as-is. Empty / invalid → returns "".
func resolveTracePath(raw string) string {
	if raw == "" {
		return ""
	}
	// If the path ends with a separator OR refers to an existing
	// directory, rotate daily.
	if len(raw) > 0 && (raw[len(raw)-1] == '/' || raw[len(raw)-1] == os.PathSeparator) {
		return filepath.Join(raw, time.Now().UTC().Format("2006-01-02")+".jsonl")
	}
	if st, err := os.Stat(raw); err == nil && st.IsDir() {
		return filepath.Join(raw, time.Now().UTC().Format("2006-01-02")+".jsonl")
	}
	return raw
}

// currentWorkspace returns os.Getwd() or "" on failure. Banya-cli
// is spawned per-task with cwd set to the workspace, so this lets
// the trace be cross-referenced with `tasks.jsonl` after the fact.
func currentWorkspace() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// appendTraceWithProvider is an explicit-provider variant for future
// adapters (gemini / llm-server) that don't want to hard-code the
// provider string. Not used yet; kept so future code can opt in
// without editing this file's public surface.
func appendTraceWithProvider(
	path, provider string,
	params protocol.LlmChatParams,
	completion, model string,
	elapsed time.Duration,
) {
	if provider == "" {
		provider = "unknown"
	}
	record := traceRecord{
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		Provider:     provider,
		Model:        model,
		Messages:     params.Messages,
		Tools:        params.Tools,
		Completion:   completion,
		ElapsedMs:    elapsed.Milliseconds(),
		WorkspaceCwd: currentWorkspace(),
	}
	target := resolveTracePath(path)
	if target == "" {
		return
	}
	line, err := json.Marshal(record)
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
	// silence "declared and not used" — fmt is needed if we ever
	// switch to formatted errors here. Kept in import block.
	_ = fmt.Sprintf
}
