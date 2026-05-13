.PHONY: all protocol server client tools image image-server image-broker image-agent test clean install help lint fmt vet deps

VERSION ?= $(shell (git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev) | tr -cd 'a-zA-Z0-9._-')

# Default target
all: protocol server client tools

# Help target
help:
	@echo "Demarkus Build Targets:"
	@echo "  all       - Build protocol, server, and client"
	@echo "  protocol  - Build protocol library"
	@echo "  server    - Build demarkus-server"
	@echo "  client    - Build demarkus TUI client"
	@echo "  tools     - Build broker, token, publish (tools/bin/)"
	@echo "  image     - Build server + broker + agent container images (TAG overridable)"
	@echo "  test      - Run all tests"
	@echo "  lint      - Run golangci-lint on all modules"
	@echo "  clean     - Remove build artifacts"
	@echo "  install   - Install binaries to /usr/local/bin"
	@echo ""
	@echo "Development:"
	@echo "  run-server - Start dev server with docs site"
	@echo "  run-client - Fetch a document (set URL=mark://...)"
	@echo "  run-tui    - Start TUI browser (set URL=mark://...)"
	@echo "  run-mcp    - Start MCP server"

# Build protocol library
protocol:
	@echo "Building protocol library..."
	cd protocol && go build ./...
	@echo "✓ Protocol library built"

# Build server
server: protocol
	@echo "Building demarkus-server..."
	cd server && go build -o bin/demarkus-server ./cmd/demarkus-server
	@echo "✓ Server built: server/bin/demarkus-server"

# Build client
client: protocol
	@echo "Building demarkus client..."
	cd client && go build -o bin/demarkus ./cmd/demarkus
	cd client && go build -o bin/demarkus-tui ./cmd/demarkus-tui
	cd client && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-mcp ./cmd/demarkus-mcp
	cd client && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-agent ./cmd/demarkus-agent
	@echo "✓ Client built: client/bin/demarkus, client/bin/demarkus-tui, client/bin/demarkus-mcp, client/bin/demarkus-agent"

# Build tools
tools: protocol
	@echo "Building tools..."
	cd tools && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-broker  ./demarkus-broker
	cd tools && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-token   ./demarkus-token
	cd tools && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-publish ./demarkus-publish
	@echo "✓ Tools built: tools/bin/{demarkus-broker, demarkus-token, demarkus-publish}"

# Build container images. One image per deployable service so each pod
# carries only the binaries it needs at runtime. Admin CLIs are NOT
# bundled — they ship as standalone binaries via goreleaser archives.
#
# Each Dockerfile uses repo root as build context so it can reach across
# go module boundaries (server image needs both server/ and client/ for
# the CLI used by exec probes).
IMAGE_REGISTRY ?= ghcr.io/latebit-io
TAG            ?= dev

image: image-server image-broker image-agent

image-server:
	@echo "Building $(IMAGE_REGISTRY)/demarkus-server:$(TAG)..."
	docker build --build-arg VERSION=$(VERSION) -f server/Dockerfile -t $(IMAGE_REGISTRY)/demarkus-server:$(TAG) .
	@echo "✓ Image built: $(IMAGE_REGISTRY)/demarkus-server:$(TAG)"

image-broker:
	@echo "Building $(IMAGE_REGISTRY)/demarkus-broker:$(TAG)..."
	docker build --build-arg VERSION=$(VERSION) -f tools/demarkus-broker/Dockerfile -t $(IMAGE_REGISTRY)/demarkus-broker:$(TAG) .
	@echo "✓ Image built: $(IMAGE_REGISTRY)/demarkus-broker:$(TAG)"

image-agent:
	@echo "Building $(IMAGE_REGISTRY)/demarkus-agent:$(TAG)..."
	docker build --build-arg VERSION=$(VERSION) -f client/cmd/demarkus-agent/Dockerfile -t $(IMAGE_REGISTRY)/demarkus-agent:$(TAG) .
	@echo "✓ Image built: $(IMAGE_REGISTRY)/demarkus-agent:$(TAG)"

# Run tests
test:
	@echo "Running tests..."
	@cd protocol && go test ./... && echo "✓ Protocol tests passed"
	@cd server && go test ./... && echo "✓ Server tests passed"
	@cd client && go test ./... && echo "✓ Client tests passed"
	@cd tools && go test ./... && echo "✓ Tools tests passed"

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf server/bin client/bin tools/bin
	@cd protocol && go clean
	@cd server && go clean
	@cd client && go clean
	@cd tools && go clean
	@echo "✓ Clean complete"

# Install binaries
install: all
	@echo "Installing binaries..."
	@cp server/bin/demarkus-server /usr/local/bin/
	@cp client/bin/demarkus /usr/local/bin/
	@echo "✓ Installed to /usr/local/bin/"

URL ?= mark://localhost:6309/index.md

# Run server (for development)
run-server: server
	./server/bin/demarkus-server -root ./docs/site

# Run client (for development)
run-client: client
	./client/bin/demarkus --insecure $(URL)

# Run TUI (for development)
run-tui: client
	./client/bin/demarkus-tui --insecure $(URL)

# Run MCP server (for development)
run-mcp: client
	./client/bin/demarkus-mcp -host mark://localhost:6309 -insecure

# Lint code
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "Error: golangci-lint is not installed."; \
		echo "Install it: https://golangci-lint.run/welcome/install/"; \
		exit 1; \
	fi
	@echo "Linting code..."
	@cd protocol && golangci-lint run ./...
	@cd server && golangci-lint run ./...
	@cd client && golangci-lint run ./...
	@cd tools && golangci-lint run ./...
	@echo "✓ Code linted"

# Format code
fmt:
	@echo "Formatting code..."
	@cd protocol && go fmt ./...
	@cd server && go fmt ./...
	@cd client && go fmt ./...
	@cd tools && go fmt ./...
	@echo "✓ Code formatted"

# Vet code
vet:
	@echo "Vetting code..."
	@cd protocol && go vet ./...
	@cd server && go vet ./...
	@cd client && go vet ./...
	@cd tools && go vet ./...
	@echo "✓ Code vetted"

# Update dependencies
deps:
	@echo "Updating dependencies..."
	@cd protocol && go mod tidy
	@cd server && go mod tidy && go mod download
	@cd client && go mod tidy && go mod download
	@cd tools && go mod tidy && go mod download
	@echo "✓ Dependencies updated"
