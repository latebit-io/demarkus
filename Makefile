.PHONY: all protocol server knowledge-server client tools knowledge image image-server image-knowledge image-agent test clean install help lint fmt vet vuln fuzz smoke deps

VERSION ?= $(shell (git describe --tags --match 'v[0-9]*' --always --dirty 2>/dev/null || echo dev) | tr -cd 'a-zA-Z0-9._-')

# Every Go module; per-module targets loop over this list. Build order is `all`.
MODULES := protocol server client tools knowledge

# Default target
all: protocol server client tools knowledge

# Help target
help:
	@echo "Demarkus Build Targets:"
	@echo "  all       - Build protocol, server, client, tools, and knowledge"
	@echo "  protocol  - Build protocol library"
	@echo "  server    - Build demarkus-server (no external database deps)"
	@echo "  knowledge-server - Build demarkus-knowledge-server (multi-world GCS backend)"
	@echo "  client    - Build demarkus TUI client"
	@echo "  tools     - Build token, publish (tools/bin/)"
	@echo "  knowledge - Build demarkus-knowledge, the knowledge server and broker in one process (knowledge/bin/)"
	@echo "  image     - Build runtime container images (TAG overridable)"
	@echo "  image-knowledge - Build the demarkus-knowledge image"
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

knowledge-server: protocol
	@echo "Building knowledge server..."
	cd server && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-knowledge-server ./cmd/demarkus-knowledge-server
	@echo "✓ Built: server/bin/demarkus-knowledge-server"

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
	cd tools && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-token   ./demarkus-token
	cd tools && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-publish ./demarkus-publish
	@echo "✓ Tools built: tools/bin/{demarkus-token, demarkus-publish}"

# Build the composed knowledge binary
knowledge: protocol
	@echo "Building demarkus-knowledge..."
	cd knowledge && go build -ldflags "-X main.version=$(VERSION)" -o bin/demarkus-knowledge ./cmd/demarkus-knowledge
	@echo "✓ Knowledge built: knowledge/bin/demarkus-knowledge"

# Build container images. One image per deployable service so each pod
# carries only the binaries it needs at runtime. Admin CLIs are NOT
# bundled — they ship as standalone binaries via goreleaser archives.
#
# Each Dockerfile expects pre-built binaries in dist/docker/<arch>/ —
# the targets below cross-compile natively (fast — no QEMU) for HOST_ARCH
# before invoking `docker build`. Multi-arch matrix builds happen in the
# release workflow via the same staging pattern.
IMAGE_REGISTRY ?= ghcr.io/latebit-io
TAG            ?= dev
HOST_ARCH      ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/;s/armv7l/arm/')

image: image-server image-knowledge image-agent

image-server:
	@echo "Building $(IMAGE_REGISTRY)/demarkus-server:$(TAG) for linux/$(HOST_ARCH)..."
	@mkdir -p dist/docker/$(HOST_ARCH)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HOST_ARCH) go build -C server -ldflags "-s -w -X main.version=$(VERSION)" -o ../dist/docker/$(HOST_ARCH)/demarkus-server ./cmd/demarkus-server
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HOST_ARCH) go build -C client -ldflags "-s -w -X main.version=$(VERSION)" -o ../dist/docker/$(HOST_ARCH)/demarkus ./cmd/demarkus
	docker build --build-arg TARGETARCH=$(HOST_ARCH) -f server/Dockerfile -t $(IMAGE_REGISTRY)/demarkus-server:$(TAG) .
	@echo "✓ Image built: $(IMAGE_REGISTRY)/demarkus-server:$(TAG)"

image-knowledge:
	@echo "Building $(IMAGE_REGISTRY)/demarkus-knowledge:$(TAG) for linux/$(HOST_ARCH)..."
	@mkdir -p dist/docker/$(HOST_ARCH)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HOST_ARCH) go build -C knowledge -ldflags "-s -w -X main.version=$(VERSION)" -o ../dist/docker/$(HOST_ARCH)/demarkus-knowledge ./cmd/demarkus-knowledge
	docker build --build-arg TARGETARCH=$(HOST_ARCH) -f knowledge/cmd/demarkus-knowledge/Dockerfile -t $(IMAGE_REGISTRY)/demarkus-knowledge:$(TAG) .
	@echo "✓ Image built: $(IMAGE_REGISTRY)/demarkus-knowledge:$(TAG)"

image-agent:
	@echo "Building $(IMAGE_REGISTRY)/demarkus-agent:$(TAG) for linux/$(HOST_ARCH)..."
	@mkdir -p dist/docker/$(HOST_ARCH)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HOST_ARCH) go build -C client -ldflags "-s -w -X main.version=$(VERSION)" -o ../dist/docker/$(HOST_ARCH)/demarkus-agent ./cmd/demarkus-agent
	docker build --build-arg TARGETARCH=$(HOST_ARCH) -f client/cmd/demarkus-agent/Dockerfile -t $(IMAGE_REGISTRY)/demarkus-agent:$(TAG) .
	@echo "✓ Image built: $(IMAGE_REGISTRY)/demarkus-agent:$(TAG)"

# Run tests
test:
	@echo "Running tests..."
	@for mod in $(MODULES); do \
		(cd $$mod && go test -race ./...) || exit 1; \
		echo "✓ $$mod tests passed"; \
	done

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf server/bin client/bin tools/bin knowledge/bin dist
	@for mod in $(MODULES); do (cd $$mod && go clean); done
	@echo "✓ Clean complete"

# Install binaries
install: server client
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
	@for mod in $(MODULES); do \
		echo "Linting $$mod..."; \
		(cd $$mod && golangci-lint run ./...) || exit 1; \
	done
	@echo "✓ Code linted"

# Format code
fmt:
	@echo "Formatting code..."
	@for mod in $(MODULES); do (cd $$mod && go fmt ./...) || exit 1; done
	@echo "✓ Code formatted"

# Vet code
vet:
	@echo "Vetting code..."
	@for mod in $(MODULES); do (cd $$mod && go vet ./...) || exit 1; done
	@echo "✓ Code vetted"

# Report known vulnerabilities reachable from our code
vuln:
	@for mod in $(MODULES); do \
		echo "govulncheck $$mod..."; \
		(cd $$mod && go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...) || exit 1; \
	done

# Short fuzz pass over the parsers of untrusted bytes; seeds alone run under `make test`
FUZZTIME ?= 20s
fuzz:
	@cd protocol && for t in .:FuzzParseRequest .:FuzzParseResponse .:FuzzRequestRoundTrip \
		./storefmt:FuzzStoredVersionRoundTrip ./storefmt:FuzzInspectStoredVersion \
		./mdoutline:FuzzHeadings; do \
		go test -run '^$$' -fuzz "^$${t#*:}\$$" -fuzztime $(FUZZTIME) "$${t%%:*}" || exit 1; \
	done
	@cd client && go test -run '^$$' -fuzz '^FuzzParseBytes$$' -fuzztime $(FUZZTIME) ./token

# Built binaries over real QUIC; the knowledge half needs Docker.
smoke: server knowledge-server client tools
	@bash scripts/smoke.sh

# Update dependencies
deps:
	@echo "Updating dependencies..."
	@for mod in $(MODULES); do (cd $$mod && go mod tidy && go mod download) || exit 1; done
	@echo "✓ Dependencies updated"
