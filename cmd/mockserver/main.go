// Mock server that simulates the code agent API for development and testing.
// Run: go run ./cmd/mockserver
//
// Behavior is determined by keywords in the user's message:
//   - Default:          Streams a markdown response
//   - Contains "file":  Simulates a file creation tool call
//   - Contains "run":   Simulates a shell command tool call
//   - Contains "rm":    Triggers an approval flow (high risk)
//   - Contains "error": Returns an error event
//   - Contains "help":  Returns a help message
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
	"github.com/google/uuid"
)

func main() {
	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", handleHealth)
	mux.HandleFunc("/api/v1/chat", handleChat)
	mux.HandleFunc("/api/v1/approval", handleApproval)

	log.Printf("Mock server starting on :%s", port)
	log.Printf("Endpoints:")
	log.Printf("  GET  /api/v1/health")
	log.Printf("  POST /api/v1/chat    (SSE stream)")
	log.Printf("  POST /api/v1/approval")
	log.Printf("")
	log.Printf("Keywords for testing:")
	log.Printf("  (default)  -> streamed markdown response")
	log.Printf("  'file'     -> file creation tool call")
	log.Printf("  'run CMD'  -> execute shell command tool call")
	log.Printf("  'rm'       -> approval required (high risk)")
	log.Printf("  'error'    -> error event")
	log.Printf("  'help'     -> help message")

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// --- Handlers ---

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var resp protocol.ApprovalResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[approval] tool_call=%s approved=%v", resp.ToolCallID, resp.Approved)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req protocol.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("[chat] session=%s message=%q workdir=%s", req.SessionID, req.Message, req.WorkDir)

	// Set up SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	msg := strings.ToLower(req.Message)

	// Send stream_start
	sendSSE(w, flusher, protocol.EventStreamStart, map[string]string{
		"session_id": req.SessionID,
	})
	time.Sleep(100 * time.Millisecond)

	// Route to scenario based on keywords
	switch {
	case strings.Contains(msg, "error"):
		scenarioError(w, flusher, req)
	case strings.Contains(msg, "rm ") || strings.Contains(msg, "delete"):
		scenarioApproval(w, flusher, req)
	case strings.Contains(msg, "run ") || strings.Contains(msg, "execute "):
		scenarioShellCommand(w, flusher, req)
	case strings.Contains(msg, "file") || strings.Contains(msg, "create"):
		scenarioFileCreate(w, flusher, req)
	case strings.Contains(msg, "help"):
		scenarioHelp(w, flusher, req)
	default:
		scenarioChat(w, flusher, req)
	}

	// Send done
	sendSSE(w, flusher, protocol.EventDone, map[string]string{
		"session_id": req.SessionID,
	})
}

// --- Scenarios ---

// scenarioChat streams a conversational markdown response.
func scenarioChat(w http.ResponseWriter, f http.Flusher, req protocol.ChatRequest) {
	response := generateChatResponse(req.Message)
	streamText(w, f, req.SessionID, response)
}

// scenarioHelp streams a help message.
func scenarioHelp(w http.ResponseWriter, f http.Flusher, req protocol.ChatRequest) {
	help := `Banya는 AI 코드 에이전트 CLI입니다. 다음과 같은 작업을 도와드릴 수 있습니다:

## 사용 가능한 기능
- **파일 작업**: 프로젝트의 파일 생성, 수정, 읽기
- **셸 명령**: 명령 실행 및 출력 분석
- **코드 지원**: 코드 작성, 리뷰, 리팩토링
- **프로젝트 분석**: 프로젝트 구조 및 의존성 파악

## 사용법
- 자연스럽게 입력하면 의도를 분석하여 처리합니다
- 위험한 명령 실행 전에는 승인을 요청합니다
- ` + "`/clear`" + ` — 대화 초기화
- ` + "`/quit`" + ` — 종료

## 테스트 키워드 (Mock 서버)
- ` + "`run <명령>`" + ` — 셸 명령 실행
- ` + "`file`" + ` 또는 ` + "`create`" + ` — 파일 생성 시뮬레이션
- ` + "`rm`" + ` 또는 ` + "`delete`" + ` — 승인 플로우 테스트
- ` + "`error`" + ` — 에러 시뮬레이션`

	streamText(w, f, req.SessionID, help)
}

// scenarioFileCreate simulates a file creation tool call.
func scenarioFileCreate(w http.ResponseWriter, f http.Flusher, req protocol.ChatRequest) {
	ko := isKorean(req.Message)
	if ko {
		streamText(w, f, req.SessionID, "새 파일을 생성하겠습니다. 준비 중...\n\n")
	} else {
		streamText(w, f, req.SessionID, "I'll create a new file for you. Let me set that up.\n\n")
	}
	time.Sleep(200 * time.Millisecond)

	// Phase 2: tool call
	tcID := uuid.New().String()
	sendSSE(w, f, protocol.EventToolCallStart, protocol.ToolCall{
		ID:     tcID,
		Name:   "create_file",
		Status: protocol.ToolCallRunning,
		Args: map[string]any{
			"path":    "hello.py",
			"content": "#!/usr/bin/env python3\nprint('Hello from Banya!')\n",
		},
	})
	time.Sleep(800 * time.Millisecond)

	sendSSE(w, f, protocol.EventToolCallDone, protocol.ToolCall{
		ID:     tcID,
		Name:   "create_file",
		Status: protocol.ToolCallDone,
		Result: "File created: hello.py (2 lines)",
	})
	time.Sleep(200 * time.Millisecond)

	// Phase 3: summary
	if ko {
		streamText(w, f, req.SessionID, "\n`hello.py` 파일을 생성했습니다. 다음 명령으로 실행할 수 있습니다:\n\n```bash\npython3 hello.py\n```\n")
	} else {
		streamText(w, f, req.SessionID, "\nI've created `hello.py` with a simple Python script. You can run it with:\n\n```bash\npython3 hello.py\n```\n")
	}
}

// scenarioShellCommand simulates executing a shell command.
func scenarioShellCommand(w http.ResponseWriter, f http.Flusher, req protocol.ChatRequest) {
	// Extract command after "run " or "execute "
	command := req.Message
	for _, prefix := range []string{"run ", "Run ", "execute ", "Execute "} {
		if idx := strings.Index(command, prefix); idx >= 0 {
			command = strings.TrimSpace(command[idx+len(prefix):])
			break
		}
	}

	if isKorean(req.Message) {
		streamText(w, f, req.SessionID, fmt.Sprintf("`%s` 명령을 실행하겠습니다.\n\n", command))
	} else {
		streamText(w, f, req.SessionID, fmt.Sprintf("I'll run the command `%s` for you.\n\n", command))
	}
	time.Sleep(200 * time.Millisecond)

	// Tool call start
	tcID := uuid.New().String()
	sendSSE(w, f, protocol.EventToolCallStart, protocol.ToolCall{
		ID:     tcID,
		Name:   "shell_exec",
		Status: protocol.ToolCallRunning,
		Args: map[string]any{
			"command": command,
		},
	})

	// Actually execute the command
	cmd := exec.Command("bash", "-c", command)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	output, err := cmd.CombinedOutput()
	time.Sleep(300 * time.Millisecond)

	if err != nil {
		sendSSE(w, f, protocol.EventToolCallDone, protocol.ToolCall{
			ID:     tcID,
			Name:   "shell_exec",
			Status: protocol.ToolCallFailed,
			Result: string(output),
			Error:  err.Error(),
		})
		time.Sleep(200 * time.Millisecond)
		if isKorean(req.Message) {
			streamText(w, f, req.SessionID, fmt.Sprintf("\n명령 실행 실패: `%s`\n\n```\n%s```\n", err.Error(), string(output)))
		} else {
			streamText(w, f, req.SessionID, fmt.Sprintf("\nThe command failed with error: `%s`\n\n```\n%s```\n", err.Error(), string(output)))
		}
	} else {
		result := string(output)
		sendSSE(w, f, protocol.EventToolCallDone, protocol.ToolCall{
			ID:     tcID,
			Name:   "shell_exec",
			Status: protocol.ToolCallDone,
			Result: result,
		})
		time.Sleep(200 * time.Millisecond)
		if isKorean(req.Message) {
			streamText(w, f, req.SessionID, fmt.Sprintf("\n명령이 성공적으로 완료되었습니다:\n\n```\n%s```\n", result))
		} else {
			streamText(w, f, req.SessionID, fmt.Sprintf("\nCommand completed successfully:\n\n```\n%s```\n", result))
		}
	}
}

// scenarioApproval simulates a high-risk command that needs approval.
func scenarioApproval(w http.ResponseWriter, f http.Flusher, req protocol.ChatRequest) {
	ko := isKorean(req.Message)
	if ko {
		streamText(w, f, req.SessionID, "위험할 수 있는 명령입니다. 실행 전에 승인이 필요합니다.\n\n")
	} else {
		streamText(w, f, req.SessionID, "This operation involves a potentially destructive command. I need your approval before proceeding.\n\n")
	}
	time.Sleep(300 * time.Millisecond)

	desc := "Execute a potentially destructive file operation"
	if ko {
		desc = "잠재적으로 위험한 파일 작업 실행"
	}

	tcID := uuid.New().String()
	sendSSE(w, f, protocol.EventApprovalNeeded, protocol.ApprovalRequest{
		ToolCallID:  tcID,
		ToolName:    "shell_exec",
		Description: desc,
		Command:     req.Message,
		Risk:        "high",
	})

	time.Sleep(2 * time.Second)
	if ko {
		streamText(w, f, req.SessionID, "\n승인 플로우가 완료되었습니다. 실제 서버에서는 사용자 응답을 대기합니다.\n")
	} else {
		streamText(w, f, req.SessionID, "\nApproval flow completed. In production, the server would wait for your response before proceeding.\n")
	}
}

// scenarioError simulates an error event.
func scenarioError(w http.ResponseWriter, f http.Flusher, req protocol.ChatRequest) {
	ko := isKorean(req.Message)
	if ko {
		streamText(w, f, req.SessionID, "요청을 처리 중입니다...\n")
	} else {
		streamText(w, f, req.SessionID, "Processing your request...\n")
	}
	time.Sleep(500 * time.Millisecond)

	errMsg := "This is a simulated error for testing. The agent encountered an unexpected condition."
	if ko {
		errMsg = "테스트용 시뮬레이션 에러입니다. 에이전트가 예상치 못한 상태를 만났습니다."
	}
	sendSSE(w, f, protocol.EventError, protocol.ErrorData{
		Code:    "MOCK_ERROR",
		Message: errMsg,
	})
	time.Sleep(200 * time.Millisecond)

	if ko {
		streamText(w, f, req.SessionID, "\n처리 중 에러가 발생했습니다. 다시 시도하거나 질문을 바꿔주세요.\n")
	} else {
		streamText(w, f, req.SessionID, "\nI encountered an error while processing. Please try again or rephrase your request.\n")
	}
}

// --- Helpers ---

// streamText sends content as rune-by-rune SSE deltas (simulating typing).
// Uses rune slicing to avoid splitting multi-byte UTF-8 characters (e.g. Korean).
func streamText(w http.ResponseWriter, f http.Flusher, sessionID, text string) {
	runes := []rune(text)
	i := 0
	for i < len(runes) {
		chunkSize := 1 + rand.Intn(3)
		if i+chunkSize > len(runes) {
			chunkSize = len(runes) - i
		}
		chunk := string(runes[i : i+chunkSize])

		sendSSE(w, f, protocol.EventContentDelta, protocol.ContentDelta{
			Content: chunk,
		})

		i += chunkSize
		time.Sleep(time.Duration(15+rand.Intn(35)) * time.Millisecond)
	}
}

// sendSSE writes a single SSE event.
func sendSSE(w http.ResponseWriter, f http.Flusher, eventType protocol.EventType, data any) {
	evt := protocol.ServerEvent{
		Type: eventType,
		Data: data,
	}
	jsonData, err := json.Marshal(evt)
	if err != nil {
		log.Printf("Error marshaling SSE event: %v", err)
		return
	}

	fmt.Fprintf(w, "event:%s\ndata:%s\n\n", eventType, string(jsonData))
	f.Flush()
}

// isKorean detects if the message contains Korean characters.
func isKorean(text string) bool {
	for _, r := range text {
		// Hangul Syllables (AC00-D7AF), Hangul Jamo (1100-11FF), Hangul Compat Jamo (3130-318F)
		if (r >= 0xAC00 && r <= 0xD7AF) || (r >= 0x1100 && r <= 0x11FF) || (r >= 0x3130 && r <= 0x318F) {
			return true
		}
	}
	return false
}

// generateChatResponse creates a context-aware response based on the user's message.
// Responds in Korean if the input contains Korean characters.
func generateChatResponse(userMsg string) string {
	msg := strings.ToLower(userMsg)
	ko := isKorean(userMsg)

	switch {
	case strings.Contains(msg, "hello") || strings.Contains(msg, "hi") || strings.Contains(msg, "안녕"):
		if ko {
			return `안녕하세요! 저는 **Banya**, AI 코드 에이전트입니다. 다음과 같은 작업을 도와드릴 수 있습니다:

- 코드 작성 및 수정
- 셸 명령 실행
- 프로젝트 구조 분석
- 디버깅

오늘 어떤 작업을 도와드릴까요?`
		}
		return `Hello! I'm **Banya**, your AI code agent. I can help you with:

- Writing and modifying code
- Executing shell commands
- Analyzing your project structure
- Debugging issues

What would you like to work on today?`

	case strings.Contains(msg, "프로젝트") || strings.Contains(msg, "구조") || strings.Contains(msg, "project") || strings.Contains(msg, "structure"):
		if ko {
			return `프로젝트 구조를 분석해 보겠습니다.

현재 **Go 프로젝트**로 보이며, 다음과 같은 구조입니다:

` + "```" + `
.
├── cmd/           # 애플리케이션 진입점
├── internal/      # 내부 패키지
├── pkg/           # 공개 라이브러리
├── configs/       # 설정 파일
├── go.mod         # Go 모듈 정의
└── Makefile       # 빌드 자동화
` + "```" + `

표준 Go 프로젝트 레이아웃을 따르고 있습니다. 특정 부분을 더 자세히 살펴볼까요?`
		}
		return `Let me analyze your project structure.

Based on what I can see, this appears to be a **Go project** with the following layout:

` + "```" + `
.
├── cmd/           # Application entrypoints
├── internal/      # Private application code
├── pkg/           # Public library code
├── configs/       # Configuration files
├── go.mod         # Go module definition
└── Makefile       # Build automation
` + "```" + `

This follows the standard Go project layout. Would you like me to dive deeper into any specific part?`

	case strings.Contains(msg, "테스트") || strings.Contains(msg, "test"):
		if ko {
			return `테스트 작성을 도와드리겠습니다. Go의 기본 테스트 구조입니다:

` + "```go" + `
package mypackage_test

import (
    "testing"
)

func TestExample(t *testing.T) {
    got := Add(2, 3)
    want := 5
    if got != want {
        t.Errorf("Add(2, 3) = %d, want %d", got, want)
    }
}
` + "```" + `

실행 방법:
` + "```bash" + `
go test ./... -v
` + "```" + `

프로젝트에 테스트 파일을 생성해 드릴까요?`
		}
		return `I'll help you set up tests. Here's a basic test structure for Go:

` + "```go" + `
package mypackage_test

import (
    "testing"
)

func TestExample(t *testing.T) {
    got := Add(2, 3)
    want := 5
    if got != want {
        t.Errorf("Add(2, 3) = %d, want %d", got, want)
    }
}
` + "```" + `

Run tests with:
` + "```bash" + `
go test ./... -v
` + "```" + `

Would you like me to create test files for your project?`

	default:
		if ko {
			return fmt.Sprintf(`메시지를 받았습니다: *"%s"*

현재 Mock 서버가 시뮬레이션 응답을 보내고 있습니다. 실제 코드 에이전트에서는:

1. **의도 분석** — 자체 의도 분석 모듈로 사용자 요청을 파악합니다
2. **계획 수립** — 요청을 처리하기 위한 단계를 계획합니다
3. **도구 실행** — 파일 작업, 셸 명령 등을 수행합니다
4. **결과 보고** — 리치 포맷팅으로 결과를 표시합니다

테스트 키워드:
- **"help"** — 상세 도움말
- **"run ls -la"** — 셸 명령 실행
- **"파일 생성"** — 파일 생성 시뮬레이션
- **"삭제"** — 승인 플로우 테스트
- **"error"** — 에러 시뮬레이션`, userMsg)
		}
		return fmt.Sprintf(`I received your message: *"%s"*

I'm the mock server responding with a simulated agent reply. In production, the real code agent would:

1. **Analyze** your intent using the intent analysis module
2. **Plan** the necessary steps to fulfill your request
3. **Execute** tool calls (file operations, shell commands, etc.)
4. **Report** results back to you with rich formatting

Try these keywords for different behaviors:
- **"help"** — detailed help message
- **"run ls -la"** — execute a shell command
- **"create file"** — simulate file creation
- **"rm something"** — trigger approval flow
- **"error"** — simulate an error`, userMsg)
	}
}
