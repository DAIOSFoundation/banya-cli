APP_NAME := banya
VERSION := 0.1.0
BUILD_DIR := build
GO := $(HOME)/go-install/go/bin/go
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build build-mock install clean test lint run dev help

all: build

## build: Build the CLI binary
build:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(APP_NAME) ./cmd/banya

## build-mock: Build the mock server
build-mock:
	@mkdir -p $(BUILD_DIR)
	$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/mockserver ./cmd/mockserver

## install: Install to $GOPATH/bin
install:
	$(GO) install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/banya

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
