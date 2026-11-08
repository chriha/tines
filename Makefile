VERSION ?= dev

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/tines ./cmd/tines

test:
	go test ./...

install:
	go install -ldflags "-X main.version=$(VERSION)" ./cmd/tines

.PHONY: build test install
