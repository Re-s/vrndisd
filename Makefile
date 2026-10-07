# vrndis-go Makefile
BINARY_NAME = vrndisd
VERSION ?= 1.0.0
LDFLAGS = -s -w -X main.Version=

.PHONY: all build build-arm64 build-armv6 build-armv7 clean

all: build

build-amd64:
	@mkdir -p ./build/bin/amd64
	GOOS=linux GOARCH=amd64 go build -o ./build/bin/amd64/ -ldflags  ./cmd/

build-arm64:
	@mkdir -p ./build/bin/arm64
	GOOS=linux GOARCH=arm64 go build -o ./build/bin/arm64/ -ldflags  ./cmd/

build-armv6:
	@mkdir -p ./build/bin/armv6
	GOOS=linux GOARCH=arm GOARM=6 go build -o ./build/bin/armv6/ -ldflags  ./cmd/

build-armv7:
	@mkdir -p ./build/bin/armv7
	GOOS=linux GOARCH=arm GOARM=7 go build -o ./build/bin/armv7/ -ldflags  ./cmd/

clean:
	rm -rf ./build/bin/*

test:
	go test ./... -v -count=1
