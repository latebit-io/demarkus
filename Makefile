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
	@echo "✓ Server built: server/bin/demarkus-server"

# Build client
client: protocol
	@echo "Building demarkus client..."
	cd client && go build -o bin/demarkus ./cmd/demarkus
	@echo "✓ Client built: client/bin/demarkus"

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

# Run server (for development)
run-server: server
	cd server && ./bin/demarkus-server --config config.example.toml

# Run client (for development)
run-client: client
	cd client && ./bin/demarkus

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
