.PHONY: build format image image-v2 test test-race vet

build:
	go build -o fern ./cmd/fern

format:
	gofmt -w cmd internal

image:
	docker build -t fern/opencode:dev images/opencode

image-v2:
	docker build -t fern/opencode-v2:dev images/opencode-v2

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...
