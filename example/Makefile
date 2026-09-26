.PHONY: build test test-integration lint verify run docker-build

build:
	mkdir -p _output
	go mod download
	go build -o _output/llm-file-gateway ./cmd/llm-file-gateway

test:
	go test -short ./internal/... ./cmd/...

test-integration:
	go test ./test/...

# golangci-lint binary location: prefer the one on PATH, otherwise the one
# installed into GOPATH/bin by `go install`.
GOLANGCI_LINT := $(shell command -v golangci-lint 2>/dev/null || echo $(shell go env GOPATH)/bin/golangci-lint)

# lint runs the configured Go linters. When golangci-lint is not installed it
# is installed first, so the target never silently passes without checks.
lint:
	@if [ ! -x "$(GOLANGCI_LINT)" ]; then \
		echo "golangci-lint not found; installing..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest; \
	fi
	"$(GOLANGCI_LINT)" run ./...

verify:
	go test -race -short ./internal/... ./cmd/...
	go vet ./...
	$(MAKE) lint

run:
	go run ./cmd/llm-file-gateway

docker-build:
	docker build -t llm-file-gateway:local .