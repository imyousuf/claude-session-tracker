BINARY := cst
BUILD_DIR := bin
GOPATH ?= $(shell go env GOPATH)
LDFLAGS := -s -w

.PHONY: build install test test-fast fmt lint clean

build:
	go build -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/cst

# Install via atomic rename: copy into the destination dir under a temp name,
# then mv over the target. rename(2) replaces the inode, so this succeeds even
# while the old binary is running (e.g. the cst daemon holds it open) — a plain
# `cp` over a busy executable fails with "Text file busy".
install: build
	cp $(BUILD_DIR)/$(BINARY) $(GOPATH)/bin/$(BINARY).new
	mv -f $(GOPATH)/bin/$(BINARY).new $(GOPATH)/bin/$(BINARY)

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
