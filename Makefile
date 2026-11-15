VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet race release

build:
	go build -o bin/nsm ./cmd/nsm

test:
	go test ./... -count=1

race:
	go test ./... -race -count=1

vet:
	go vet ./...

release:
	./scripts/build-release.sh $(VERSION)
