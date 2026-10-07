.PHONY: build format image-background-source lint release test test-background-qualification test-critical test-race test-deployment vet

build:
	go build -o fern ./cmd/fern

format:
	gofmt -w cmd internal

image-background-source:
	docker build -t fern/opencode-background-source:dev images/opencode-background-source

lint:
	golangci-lint run

# Reproducible Linux release binaries plus SHA256SUMS in dist/.
VERSION ?= dev
COMMIT ?= $(shell git rev-parse HEAD)
release:
	rm -rf dist && mkdir dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -buildvcs=false \
			-ldflags "-s -w -buildid= -X main.version=$(VERSION) -X main.commit=$(COMMIT)" \
			-o dist/fern-$(VERSION)-linux-$$arch ./cmd/fern || exit 1; \
	done
	cd dist && shasum -a 256 fern-* >SHA256SUMS

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
