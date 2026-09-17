.PHONY: build test test-integration verify run docker-build

build:
	mkdir -p _output
	go mod download
	go build -o _output/llm-file-gateway ./cmd/llm-file-gateway

test:
	go test -short ./...

test-integration:
	go test ./...

verify:
	go test -race -short ./...
	go vet ./...

run:
	go run ./cmd/llm-file-gateway

docker-build:
	docker build -t llm-file-gateway:local .