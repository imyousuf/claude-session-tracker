BINARY := cst
BUILD_DIR := bin
GOPATH ?= $(shell go env GOPATH)
LDFLAGS := -s -w

.PHONY: build install test test-fast fmt lint clean

build:
	go build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/cst

install: build
	cp $(BUILD_DIR)/$(BINARY) $(GOPATH)/bin/$(BINARY)

test:
	go test -race ./...

test-fast:
	go test ./...

fmt:
	gofmt -s -w .

lint:
	golangci-lint run

clean:
	rm -rf $(BUILD_DIR)

tidy:
	go mod tidy
