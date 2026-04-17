APP_NAME := banya
VERSION := 0.1.0
BUILD_DIR := build
GO ?= go
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

# Path to the prebuilt banya-core sidecar binaries. The cli embeds the
# binary matching the host platform (or a target platform during release).
SIDECAR_DIR ?= ../banya-core/dist
EMBED_DIR := internal/client/embedded_sidecar

# Resolve host platform sidecar name.
HOST_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
HOST_ARCH := $(shell uname -m | sed 's/x86_64/amd64/')
HOST_SIDECAR := banya-core-$(HOST_OS)-$(HOST_ARCH)

.PHONY: all build build-mock install clean test lint run dev help embed-sidecar

all: build

## embed-sidecar: Copy the host-platform banya-core binary into the embed dir
embed-sidecar:
	@mkdir -p $(EMBED_DIR)
	@if [ -f "$(SIDECAR_DIR)/$(HOST_SIDECAR)" ]; then \
		cp "$(SIDECAR_DIR)/$(HOST_SIDECAR)" "$(EMBED_DIR)/$(HOST_SIDECAR)"; \
		echo "embedded $(HOST_SIDECAR) (`du -h $(EMBED_DIR)/$(HOST_SIDECAR) | cut -f1`)"; \
	else \
		echo "WARN: $(SIDECAR_DIR)/$(HOST_SIDECAR) not found — build banya-core first (pnpm build:sidecar)"; \
		echo "WARN: cli will be built without an embedded sidecar"; \
	fi

## build: Build the CLI binary (with embedded sidecar)
build: embed-sidecar
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/banya

## build-mock: Build the mock server
build-mock:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/mockserver ./cmd/mockserver

## install: Install the cli to ~/.local/bin and register a shell alias
##          Pass PREFIX=/path to install elsewhere; pass NO_ALIAS=1 to skip rc edits
install: build
	@PREFIX="$${PREFIX:-$$HOME/.local/bin}"; \
	mkdir -p "$$PREFIX"; \
	cp "$(BUILD_DIR)/$(APP_NAME)" "$$PREFIX/$(APP_NAME)"; \
	chmod +x "$$PREFIX/$(APP_NAME)"; \
	if [ "$$(uname -s)" = "Darwin" ]; then \
		codesign --force --sign - "$$PREFIX/$(APP_NAME)" 2>/dev/null || true; \
		xattr -d com.apple.quarantine "$$PREFIX/$(APP_NAME)" 2>/dev/null || true; \
	fi; \
	echo "installed $$PREFIX/$(APP_NAME)"; \
	if [ -z "$$NO_ALIAS" ]; then \
		sh scripts/register-alias.sh "$$PREFIX/$(APP_NAME)"; \
	fi

## uninstall: Remove the installed binary and the shell alias block
uninstall:
	@PREFIX="$${PREFIX:-$$HOME/.local/bin}"; \
	rm -f "$$PREFIX/$(APP_NAME)" && echo "removed $$PREFIX/$(APP_NAME)" || true; \
	sh scripts/register-alias.sh --uninstall

## run: Build and run the CLI
run: build
	./$(BUILD_DIR)/$(APP_NAME)

## mock: Build and run the mock server
mock: build-mock
	./$(BUILD_DIR)/mockserver

## dev: Build all and run mock server + CLI together
dev: build build-mock
	@echo "Starting mock server on :8080..."
	@./$(BUILD_DIR)/mockserver &
	@sleep 1
	@./$(BUILD_DIR)/$(APP_NAME); kill %1 2>/dev/null || true

## test: Run tests
test:
	$(GO) test ./... -v

## lint: Run linters
lint:
	@command -v golangci-lint >/dev/null 2>&1 || echo "golangci-lint not installed"
	golangci-lint run ./...

## clean: Remove build artifacts
clean:
	rm -rf $(BUILD_DIR)
	$(GO) clean

## release: Build for multiple platforms
release:
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-amd64 ./cmd/banya
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-linux-arm64 ./cmd/banya
	GOOS=darwin GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-amd64 ./cmd/banya
	GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME)-darwin-arm64 ./cmd/banya

## help: Show this help
help:
	@echo "Usage: make [target]"
	@echo ""
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':'
