.PHONY: server dev cli build test seed

# Production server (OpenAI-compatible API on :8000)
server:
	go run cmd/server/main.go

# ADK dev web UI (on :8080)
dev:
	go run cmd/dev/main.go

# ADK dev UI for a specific agent
dev-agent:
	@test -n "$(AGENT)" || (echo "usage: make dev-agent AGENT=test-agent" && exit 1)
	go run cmd/dev/main.go -agent $(AGENT)

# Interactive CLI chat
cli:
	go run cmd/cli/main.go

# Interactive CLI with a specific agent
cli-agent:
	@test -n "$(AGENT)" || (echo "usage: make cli-agent AGENT=test-agent" && exit 1)
	go run cmd/cli/main.go -agent $(AGENT)

# Build all binaries
build:
	go build -o bin/server cmd/server/main.go
	go build -o bin/dev cmd/dev/main.go
	go build -o bin/cli cmd/cli/main.go

# Run tests
test:
	go test ./...

# Seed OpenSearch with sample data
seed:
	go run cmd/seed/main.go
