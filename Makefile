# bonsai-monitor
#
#   make            build bonsai-server and bonsai-client for this machine
#   make test       run the unit tests
#   make dist       cross-compile every release binary into dist/
#   make install    copy the two binaries into ~/.local/bin
#   make clean
#
# Everything is CGO_ENABLED=0, -trimpath and stripped, so the binaries in dist/
# are static and run on a machine with no Go installed.

# A tagged checkout reports the tag; a plain source tree reports the release it
# belongs to, so `-version` is never just "dev".
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 1.0.0)
LDFLAGS  := -s -w -X main.Version=$(VERSION)
DIST     := dist
GO       ?= go

# One build command, reused everywhere. CGO off means no libc dependency.
BUILD = CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'

.PHONY: all build test vet fmt dist clean install run-server simulate help \
        amd64 arm64 armv7 armv6

all: build

help:
	@echo "targets: build test vet fmt dist install clean run-server simulate"
	@echo "version: $(VERSION)"

build: bonsai-server bonsai-client

bonsai-server: $(wildcard server/*.go) go.mod go.sum
	$(BUILD) -o $@ ./server

bonsai-client: $(wildcard client/*.go) go.mod go.sum
	$(BUILD) -o $@ ./client

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w server client cmd

# --- cross-compilation -------------------------------------------------------
# linux/amd64, linux/arm64, linux/arm GOARM=7 and linux/arm GOARM=6 (old Pis).

dist: amd64 arm64 armv7 armv6
	@echo
	@echo "built into $(DIST)/:"
	@ls -1 $(DIST)

amd64:
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 $(BUILD) -o $(DIST)/bonsai-server-linux-amd64 ./server
	GOOS=linux GOARCH=amd64 $(BUILD) -o $(DIST)/bonsai-client-linux-amd64 ./client

arm64:
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=arm64 $(BUILD) -o $(DIST)/bonsai-server-linux-arm64 ./server
	GOOS=linux GOARCH=arm64 $(BUILD) -o $(DIST)/bonsai-client-linux-arm64 ./client

armv7:
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=arm GOARM=7 $(BUILD) -o $(DIST)/bonsai-server-linux-armv7 ./server
	GOOS=linux GOARCH=arm GOARM=7 $(BUILD) -o $(DIST)/bonsai-client-linux-armv7 ./client

armv6:
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=arm GOARM=6 $(BUILD) -o $(DIST)/bonsai-server-linux-armv6 ./server
	GOOS=linux GOARCH=arm GOARM=6 $(BUILD) -o $(DIST)/bonsai-client-linux-armv6 ./client

# --- convenience -------------------------------------------------------------

install: build
	@mkdir -p $(HOME)/.local/bin
	install -m 0755 bonsai-server bonsai-client $(HOME)/.local/bin/

run-server: build
	./bonsai-server

# go run ./cmd/simulate -server localhost -n 8
simulate: build
	$(GO) run ./cmd/simulate -server localhost -n 8

clean:
	rm -rf $(DIST) bonsai-server bonsai-client
