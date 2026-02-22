.PHONY: all protocol server client tools test clean install help

# Default target
all: protocol server client

# Help target
help:
	@echo "Demarkus Build Targets:"
	@echo "  all       - Build protocol, server, and client"
	@echo "  protocol  - Build protocol library"
	@echo "  server    - Build demarkus-server"
	@echo "  client    - Build demarkus TUI client"
	@echo "  tools     - Build development tools"
	@echo "  test      - Run all tests"
	@echo "  clean     - Remove build artifacts"
	@echo "  install   - Install binaries to /usr/local/bin"

# Build protocol library
protocol:
	@echo "Building protocol library..."
	cd protocol && go build ./...
	@echo "✓ Protocol library built"

# Build server
server: protocol
	@echo "Building demarkus-server..."
	cd server && go build -o bin/demarkus-server ./cmd/demarkus-server
	cd server && go build -o bin/demarkus-token ./cmd/demarkus-token
	@echo "✓ Server built: server/bin/demarkus-server, server/bin/demarkus-token"

# Build client
client: protocol
	@echo "Building demarkus client..."
	cd client && go build -o bin/demarkus ./cmd/demarkus
	cd client && go build -o bin/demarkus-tui ./cmd/demarkus-tui
	cd client && go build -o bin/demarkus-mcp ./cmd/demarkus-mcp
	@echo "✓ Client built: client/bin/demarkus, client/bin/demarkus-tui, client/bin/demarkus-mcp"

# Build tools
tools:
	@echo "Building tools..."
	cd tools && go build ./...
	@echo "✓ Tools built"

# Run tests
test:
	@echo "Running tests..."
	@cd protocol && go test ./... && echo "✓ Protocol tests passed"
	@cd server && go test ./... && echo "✓ Server tests passed"
	@cd client && go test ./... && echo "✓ Client tests passed"

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf server/bin client/bin tools/bin
	@cd protocol && go clean
	@cd server && go clean
	@cd client && go clean
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
	./server/bin/demarkus-server -root ./examples/demo-site

# Run client (for development)
run-client: client
	./client/bin/demarkus --insecure $(URL)

# Run TUI (for development)
run-tui: client
	./client/bin/demarkus-tui --insecure $(URL)

# Run MCP server (for development)
run-mcp: client
	./client/bin/demarkus-mcp -host mark://localhost:6309 -insecure

# Format code
fmt:
	@echo "Formatting code..."
	@cd protocol && go fmt ./...
	@cd server && go fmt ./...
	@cd client && go fmt ./...
	@echo "✓ Code formatted"

# Vet code
vet:
	@echo "Vetting code..."
	@cd protocol && go vet ./...
	@cd server && go vet ./...
	@cd client && go vet ./...
	@echo "✓ Code vetted"

# Update dependencies
deps:
	@echo "Updating dependencies..."
	@cd protocol && go mod tidy
	@cd server && go mod tidy && go mod download
	@cd client && go mod tidy && go mod download
	@echo "✓ Dependencies updated"
