# banya-cli

Terminal-based AI coding agent — a Go TUI + headless runner that drives `banya-core` (the LLM agent loop) against your local repo. Supports multiple model providers (self-hosted Qwen via vLLM, Gemini, Claude Opus API, and the local Claude CLI for MAX-plan users) through a uniform backend registry.

```
 ⚡❯ ┃ 파이썬으로 벽돌꺠기 게임 만들어줘
 Enter:전송  │  Ctrl+T:디버그  │  /settings:모델설정  │  /clear:초기화  │  /quit:종료 ▶
```

---

## Features

- **Interactive TUI** built on [Bubble Tea](https://github.com/charmbracelet/bubbletea) with powerline-style status bar, streaming token rendering, tool-approval flow, and Korean-friendly input.
- **Headless mode** (`banya run`) that emits every agent event as NDJSON on stdout — meant for evaluation harnesses, automation, and product integrations (e.g. vibesynth's design-to-live-app codegen).
- **Embedded sidecar**: a platform-native `banya-core` binary is compiled in via Bun SEA and auto-extracted on first run, so users don't need Node.js / bun installed.
- **Pluggable LLM backends** wired through a factory registry — swap providers at runtime with `/model <id>`, no restart.
- **LLM trace logger** (optional) that captures every chat turn as JSONL for LoRA training data collection (see `BANYA_LLM_TRACE_PATH`).
- **Gemini-backed critic loop**: patches produced by the agent are automatically reviewed and (optionally) revised before emission. Configurable thresholds per prompt mode.

---

## Install

### From source

```bash
git clone https://github.com/DAIOSFoundation/banya-cli.git
cd banya-cli
make install      # builds + copies to $GOPATH/bin
```

`make install` runs `go build` with a custom Go binary path (`$HOME/go-install/go/bin/go`) — edit the Makefile if your Go toolchain lives elsewhere.

### Binary release

Cross-compiled binaries (linux/darwin × amd64/arm64) are produced by `make release` into `build/`.

---

## Quick start

### Interactive TUI

```bash
banya
```

Opens the chat loop in the current directory. Type a request (`파이썬으로 벽돌꺠기 게임 만들어줘`) and press Enter. The agent will gather context, call tools (`read_file`, `update_file`, `run_command`, …), and stream its response. Use `/help` for the slash-command menu (or just type `/` to see the dropdown).

### Headless run (NDJSON)

```bash
banya run \
  --prompt "Fix the crash in ConfigLoader.parse — error message is 'panic: interface conversion'" \
  --workspace /path/to/repo \
  --timeout 600s
```

Every server event (stream start, content deltas, tool calls, tool results, approvals, done markers) is emitted as one-line JSON on stdout. Two extra meta markers bracket the run:

```
{"type":"meta","data":{"phase":"start","session_id":"...","elapsed_ms":0}}
…
{"type":"meta","data":{"phase":"exit","reason":"done|idle_abort|timeout|error","elapsed_ms":N}}
```

Exit codes: `0` normal done, `1` invocation/config error, `2` timeout or idle-abort, `3` sidecar RPC error.

---

## Choosing a model

Four built-in presets. Switch with `/model <id>` in the TUI, or set the default in `~/.config/banya/config.yaml`.

| id | model | cost | auth |
|---|---|---|---|
| `qwen` (default) | Banya-Qwen3.5-122B on self-hosted vLLM | free (on-prem) | `LLM_SERVER_API_KEY` |
| `gemini` | Google Gemini 3 Flash Preview via OpenAI-compat | metered | `GEMINI_KEY` |
| `claude-opus` | Anthropic Claude Opus 4.7 via OpenAI-compat beta endpoint | metered | `ANTHROPIC_API_KEY` |
| `claude-cli` | Claude Opus via local `claude -p` subprocess | **free on Claude MAX plan** | one-time `claude login` (OAuth) |

Advanced users can hand-edit `~/.config/banya/config.yaml` to point at any OpenAI-compatible endpoint not covered by the presets.

### Backend registry

Backends register themselves at package init in `internal/client/backends_init.go`:

- `llm-server` — OpenAI-compatible HTTP client (vLLM, self-hosted inference proxies)
- `gemini` — Google's `/v1beta/openai` endpoint (native OpenAI-shaped tool calling)
- `gemini-native` — experimental REST `generateContent` path
- `claude-cli` — spawn a local `claude -p` subprocess per turn (aliases: `claude`, `anthropic-cli`)

Add a new provider by dropping a file with its own `init()` into `internal/client/`.

---

## Configuration

Precedence (highest wins): CLI flags > env vars (`BANYA_*`) > `~/.config/banya/config.yaml` > defaults. Auth tokens live in `~/.config/banya/auth.json` separately so the main config can be checked into dotfiles.

### Key env vars

| env | purpose |
|---|---|
| `BANYA_MAIN_PROVIDER` | force a backend id (`qwen`, `gemini`, `claude-cli`, …) regardless of preset |
| `BANYA_MAIN_MODEL` | override the model name passed to the backend |
| `BANYA_CLAUDE_CLI_BIN` | path to the `claude` executable (claude-cli backend) |
| `BANYA_CRITIC_PROVIDER` | `gemini` (default) \| `claude-code` — which subagent runs patch review |
| `BANYA_CRITIC_MODEL` | override critic model (e.g. `gemini-2.5-pro`) |
| `BANYA_SUBAGENT_API_KEY` | shared key fallback for critic / proposer subagents |
| `BANYA_SKILL_INJECTION_MODE` | `lazy` (default) \| `eager` — inline skill bodies into the system prompt |
| `BANYA_LLM_TRACE_PATH` | directory for per-turn JSONL traces (LoRA data collection) |
| `BANYA_SIDECAR_STDERR` | `inherit` to pipe sidecar stderr to the terminal for debugging; default routes it to a log file so the TUI stays clean |

### Log files

Interactive TUI sessions redirect the sidecar's stderr and banya-cli's own warnings to `$XDG_DATA_HOME/banya/logs/sidecar-YYYYMMDD-HHMMSS.log` (or `~/.local/share/banya/logs/…` when XDG isn't set). Headless mode leaves stderr on the terminal — callers that pipe NDJSON on stdout can still tee stderr for diagnostics.

---

## Architecture

```
┌────────────┐  chat.start         ┌────────────┐  llm.chat  ┌──────────────┐
│  banya-cli ├────────────────────►│ banya-core ├───────────►│ llm backend  │
│  (TUI/Go)  │◄───ServerEvents────┤  (bun/TS)  │◄──────────┤ (HTTP / CLI) │
└────────────┘  JSON-RPC NDJSON    └────────────┘            └──────────────┘
      ▲                                 │
      │      tool.call (bi-directional) │
      └─────────────────────────────────┘
```

- **Protocol**: bidirectional NDJSON JSON-RPC on stdio. Client sends `chat.start` and `approval.respond`; sidecar replies with streaming `ServerEvent`s (`stream_start`, `content_delta`, `tool_call_*`, `approval_needed`, `done`) plus optional `llm.chat` host requests routed back to the client's LLM backend. Full schema in `pkg/protocol`.
- **State machine**: the TUI has three user-facing states — `StateReady` (accepting input), `StateStreaming` (rendering agent response), `StateApproval` (awaiting y/n on a risky tool call). Transitions driven by incoming events.
- **Critic loop**: after the agent's first turn completes with a `patch.diff`, a Gemini (or Claude Code) subagent reviews it; if the critic asks for a REVISE and rounds remain, a follow-up turn fires automatically. Budgets per mode in `internal/critic/critic.go`.
- **Sidecar embedding**: `internal/client/sidecar_embed.go` uses `//go:embed` to ship a pre-built banya-core binary per platform. On first run the binary is unpacked to `$XDG_DATA_HOME/banya/bin/`. `BANYA_CORE_BIN` overrides the embedded path (useful for local banya-core dev against a bun source tree).

---

## Development

```bash
make dev            # mock server :8080 + CLI
make test           # go test ./... -v
make lint           # golangci-lint
make build          # → build/banya
make build-mock     # → build/mockserver
make release        # cross-compile to build/banya-{linux,darwin}-{amd64,arm64}
```

### Running against a local banya-core (bun source)

Skip the embedded SEA binary and point at a bun wrapper:

```bash
cat > /tmp/banya-core-dev <<'EOF'
#!/bin/sh
exec bun run /abs/path/to/banya-core/core/bin/headless.ts "$@"
EOF
chmod +x /tmp/banya-core-dev

export BANYA_CORE_BIN=/tmp/banya-core-dev
banya run --prompt "..."
```

Any TypeScript edit in banya-core is picked up on the next `banya` invocation — no rebuild required.

### Test fixtures

Integration tests (`internal/ui/ui_test.go`) use [`teatest`](https://github.com/charmbracelet/x/tree/main/exp/teatest) to drive the TUI through a simulated terminal, with a `MockClient` standing in for the real sidecar. See `internal/client/chat_smoke_test.go` for the protocol-layer smoke.

---

## Related projects

| repo | role |
|---|---|
| [`banya-core`](https://github.com/kr-ai-dev-association/banya-core) | Bun/TypeScript agent core — LLM loop, tool registry, prompt composition, SIBDD learned-skill loader |
| [`banya-framework`](https://github.com/kr-ai-dev-association/banya-framework) | Mono-repo umbrella: evaluation harnesses, SIBDD pipeline, task adapters for SWE-bench / GSO / Web-Bench / PrototypeBench |
| [`prototypebench`](https://github.com/prototypebench/prototypebench) | Full-stack AI coding agent benchmark (React+Vite+Tailwind / FastAPI+SQLModel, 71 tasks, 32k tests) |

---

## License

MIT. See `LICENSE` (pending — issue #1 tracks adding the license file).
