.PHONY: build format image test test-race vet

build:
	go build -o fern ./cmd/fern

format:
	gofmt -w cmd internal

image:
	docker build -t fern/opencode:dev images/opencode

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...
