VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet race release check-tree

build:
	go build -o bin/nsm ./cmd/nsm

# The tree lives in an iCloud-synced folder, which drops "name 2.go" copies that
# the Go toolchain compiles. This turns that into a named failure.
check-tree:
	./scripts/check-tree.sh

test: check-tree
	go test ./... -count=1

race:
	go test ./... -race -count=1

vet:
	go vet ./...

release:
	./scripts/build-release.sh $(VERSION)
