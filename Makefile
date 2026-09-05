.PHONY: build format image-background image-background-source lint test test-background-qualification test-critical test-race test-deployment vet

build:
	go build -o fern ./cmd/fern

format:
	gofmt -w cmd internal

image-background:
	docker build -t fern/opencode-background:dev images/opencode-background

image-background-source:
	docker build -t fern/opencode-background-source:dev images/opencode-background-source

lint:
	golangci-lint run

test:
	go test ./...

test-background-qualification:
	./integration/background-run-qualification/run.sh

test-race:
	go test -race ./...

test-critical:
	./scripts/test-critical-coverage.sh

test-deployment:
	./scripts/test-deployment.sh

vet:
	go vet ./...
