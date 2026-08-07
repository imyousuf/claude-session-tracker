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
	@if command -v systemctl >/dev/null 2>&1 && \
		(systemctl --user is-enabled --quiet cst-daemon.service || systemctl --user is-active --quiet cst-daemon.service); then \
		$(GOPATH)/bin/$(BINARY) setup-daemon --enable >/dev/null; \
		echo "Refreshed and restarted cst-daemon.service with the new binary"; \
	fi

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
