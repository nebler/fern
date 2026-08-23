.PHONY: build format image lint test test-browser test-race test-deployment vet

build:
	go build -o fern ./cmd/fern

format:
	gofmt -w cmd internal

image:
	docker build -t fern/opencode:dev images/opencode

lint:
	golangci-lint run

test:
	go test ./...

test-race:
	go test -race ./...

test-browser:
	./scripts/test-browser.sh

test-deployment:
	./scripts/test-deployment.sh

vet:
	go vet ./...
