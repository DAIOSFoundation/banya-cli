package client

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// parseQwenXMLToolCalls extracts tool calls from Qwen3-Coder's native XML
// emission format. Used as a fallback when vLLM's `qwen3_coder`
// tool-call-parser fails to populate the response's `tool_calls` field
// (verified empirically: parser drops tool_calls under streaming + large
// payloads, and sometimes under non-streaming with the same large payloads
// — the exact failure modes are unstable across vLLM minor versions).
//
// Recognised pattern (with optional `<tool_call>...</tool_call>` wrapper):
//
//	<function=NAME>
//	<parameter=KEY1>
//	VALUE1
//	</parameter>
//	<parameter=KEY2>
//	VALUE2
//	</parameter>
//	</function>
//
// Returns one LlmToolCall per `<function=...>` block. Arguments are
// JSON-stringified (matching OpenAI native tool_calls format) so
// downstream consumers (HostCallbackProvider.emitToolCalls in banya-core)
// can `JSON.parse` them. ID is empty — neither vLLM nor banya-core's
// ToolParser uses it for routing in this code path.
func parseQwenXMLToolCalls(content string) []protocol.LlmToolCall {
	if content == "" {
		return nil
	}
	funcRe := regexp.MustCompile(`(?s)<function=([^>\s]+)>(.*?)</function>`)
	paramRe := regexp.MustCompile(`(?s)<parameter=([^>\s]+)>(.*?)</parameter>`)

	var calls []protocol.LlmToolCall
	for _, fm := range funcRe.FindAllStringSubmatch(content, -1) {
		name := strings.TrimSpace(fm[1])
		body := fm[2]
		args := map[string]any{}
		for _, pm := range paramRe.FindAllStringSubmatch(body, -1) {
			key := strings.TrimSpace(pm[1])
			val := strings.TrimSpace(pm[2])
			args[key] = val
		}
		argsJSON, err := json.Marshal(args)
		if err != nil {
			argsJSON = []byte("{}")
		}
		calls = append(calls, protocol.LlmToolCall{
			Name:      name,
			Arguments: string(argsJSON),
		})
	}
	return calls
}
