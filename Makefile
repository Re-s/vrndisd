# vrndis-go Makefile
BINARY_NAME = vrndisd
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X main.Version=$(VERSION) -X vrndis/internal/server.Version=$(VERSION)

.PHONY: all build-amd64 build-arm64 build-armv6 build-armv7 clean test

all: build

build-amd64:
	@mkdir -p ./build/bin/amd64
	GOOS=linux GOARCH=amd64 go build -o ./build/bin/amd64/$(BINARY_NAME) -ldflags "$(LDFLAGS)" ./cmd/vrndisd

build-arm64:
	@mkdir -p ./build/bin/arm64
	GOOS=linux GOARCH=arm64 go build -o ./build/bin/arm64/$(BINARY_NAME) -ldflags "$(LDFLAGS)" ./cmd/vrndisd

build-armv6:
	@mkdir -p ./build/bin/armv6
	GOOS=linux GOARCH=arm GOARM=6 go build -o ./build/bin/armv6/$(BINARY_NAME) -ldflags "$(LDFLAGS)" ./cmd/vrndisd

build-armv7:
	@mkdir -p ./build/bin/armv7
	GOOS=linux GOARCH=arm GOARM=7 go build -o ./build/bin/armv7/$(BINARY_NAME) -ldflags "$(LDFLAGS)" ./cmd/vrndisd

clean:
	rm -rf ./build/bin/*

test:
	go test ./... -v -count=1
