# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Banya CLI is a Go terminal-based AI code agent client. It provides an interactive TUI for communicating with a server-side code agent API via HTTP + Server-Sent Events (SSE) streaming.

## Build & Development Commands

The Makefile uses a custom Go path (`$(HOME)/go-install/go/bin/go`). All commands go through `make`.

```bash
make build        # Build CLI binary to build/banya
make build-mock   # Build mock server to build/mockserver
make test         # Run all tests (go test ./... -v)
make lint         # Run golangci-lint
make dev          # Start mock server on :8080 + CLI together
make mock         # Run mock server only
make run          # Build and run CLI
make install      # Install to $GOPATH/bin
make release      # Cross-compile for linux/darwin amd64/arm64
```

Run a single test:
```bash
$(HOME)/go-install/go/bin/go test ./internal/ui/ -v -run TestChatFlow
```

## Architecture

### Communication Flow

CLI (Bubble Tea TUI) -> HTTP POST /api/v1/chat -> SSE stream of ServerEvents -> TUI renders incrementally

The server can request tool approval mid-stream via `EventApprovalNeeded`, pausing the UI into `StateApproval` until the user responds via POST /api/v1/approval.

### Package Layout

- **cmd/banya/** - CLI entry point using Cobra. Root command runs the TUI; subcommands: version, login, logout, config, history, setup.
- **cmd/mockserver/** - Development mock API that simulates agent responses. Responds differently based on keywords in the message ("file", "run", "rm", "error", "help").
- **internal/app/** - Application bootstrap: config dirs, font check, API key resolution, Bubble Tea program init.
- **internal/client/** - `Client` interface with `HTTPClient` (production, SSE parsing) and `MockClient` (testing). The interface is the seam for dependency injection.
- **internal/config/** - Viper-based config with priority: CLI flags > env vars (`BANYA_*`) > config file (`~/.config/banya/config.yaml`) > defaults. Auth token stored separately in `~/.config/banya/auth.json`.
- **internal/session/** - Session persistence as JSON files in `~/.local/share/banya/`.
- **internal/shell/** - Local command execution with approval workflow and risk levels. Configurable allowed/blocked command lists.
- **internal/ui/** - Bubble Tea model with three states: `StateReady`, `StateStreaming`, `StateApproval`. Contains subpackages:
  - **components/** - chat, input (with Korean support), powerline status bar, tool call display, file diff view.
  - **styles/** - Color themes (dark/light) using Catppuccin.
- **pkg/protocol/** - Shared message types for client-server communication: `ChatRequest`, `ServerEvent`, `ToolCall`, `ApprovalRequest`, etc.

### Key Design Decisions

- **SSE streaming over WebSocket**: The client reads SSE events line-by-line and sends them through Go channels to the Bubble Tea update loop.
- **Client interface for testability**: `internal/client/types.go` defines the `Client` interface; `MockClient` with callback-based `OnSendMessage` enables TUI integration tests without a server.
- **TUI tests use teatest**: `internal/ui/ui_test.go` uses `charmbracelet/x/exp/teatest` for terminal simulation, injecting `MockClient` to test full interaction flows.
- **Powerline-style UI**: Status bar and input prompt use Nerd Font glyphs. `internal/setup/font.go` handles font detection and installation.

### SSE Event Types (pkg/protocol)

`stream_start` -> `content_delta`* -> `content_done` -> `done` (simple response)
`stream_start` -> `tool_call_start` -> `tool_call_delta`* -> `tool_call_done` -> `done` (tool execution)
`approval_needed` interrupts the stream and waits for user response.
