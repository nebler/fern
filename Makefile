.PHONY: build format image image-background image-background-source lint test test-browser test-critical test-race test-deployment vet

build:
	go build -o fern ./cmd/fern

format:
	gofmt -w cmd internal

image:
	docker build -t fern/opencode:dev images/opencode

image-background:
	docker build -t fern/opencode-background:dev images/opencode-background

image-background-source:
	docker build -t fern/opencode-background-source:dev images/opencode-background-source

lint:
	golangci-lint run

test:
	go test ./...

test-race:
	go test -race ./...

test-browser:
	./scripts/test-browser.sh

test-critical:
	./scripts/test-critical-coverage.sh

test-deployment:
	./scripts/test-deployment.sh

vet:
	go vet ./...
