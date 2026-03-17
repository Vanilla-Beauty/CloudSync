# CloudSync — cross-platform build & install
# Targets: build, install, clean, release, fmt, vet, test
#
# Cross-compile:
#   make build-windows   → dist/windows-amd64/
#   make build-linux     → dist/linux-amd64/
#   make build-darwin    → dist/darwin-amd64/
#   make release         → all three platforms

BINARY_CLI    := cloudsync
BINARY_DAEMON := cloudsyncd
MODULE        := $(shell grep '^module' go.mod | awk '{print $$2}')
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS       := -s -w \
	-X main.version=$(VERSION) \
	-X main.buildTime=$(BUILD_TIME)

GOBIN         := $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN         := $(shell go env GOPATH)/bin
endif

DEFAULT_INSTALL_DIR := /usr/local/bin

# ── local build ───────────────────────────────────────────────────────────────

.PHONY: build
build:
	@echo "Building $(BINARY_CLI) and $(BINARY_DAEMON)..."
	go build -ldflags="$(LDFLAGS)" -o $(BINARY_CLI) ./cmd/$(BINARY_CLI)/
	go build -ldflags="$(LDFLAGS)" -o $(BINARY_DAEMON) ./cmd/$(BINARY_DAEMON)/
	@echo "Done: ./$(BINARY_CLI)  ./$(BINARY_DAEMON)"

# ── install (Unix only — use install.ps1 on Windows) ─────────────────────────

.PHONY: install
install: build
	@echo "Installing to $(DEFAULT_INSTALL_DIR) ..."
	@mkdir -p $(DEFAULT_INSTALL_DIR)
	install -m 755 $(BINARY_CLI)    $(DEFAULT_INSTALL_DIR)/$(BINARY_CLI)
	install -m 755 $(BINARY_DAEMON) $(DEFAULT_INSTALL_DIR)/$(BINARY_DAEMON)
	@echo "Installed."

.PHONY: install-user
install-user: build
	@mkdir -p $(GOBIN)
	install -m 755 $(BINARY_CLI)    $(GOBIN)/$(BINARY_CLI)
	install -m 755 $(BINARY_DAEMON) $(GOBIN)/$(BINARY_DAEMON)
	@echo "Installed to $(GOBIN)"

# ── cross-compile ─────────────────────────────────────────────────────────────

DIST := dist

.PHONY: build-linux
build-linux:
	@mkdir -p $(DIST)/linux-amd64
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/linux-amd64/$(BINARY_CLI)    ./cmd/$(BINARY_CLI)/
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/linux-amd64/$(BINARY_DAEMON) ./cmd/$(BINARY_DAEMON)/
	@echo "→ $(DIST)/linux-amd64/"

.PHONY: build-darwin
build-darwin:
	@mkdir -p $(DIST)/darwin-amd64
	GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/darwin-amd64/$(BINARY_CLI)    ./cmd/$(BINARY_CLI)/
	GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/darwin-amd64/$(BINARY_DAEMON) ./cmd/$(BINARY_DAEMON)/
	@echo "→ $(DIST)/darwin-amd64/"

.PHONY: build-darwin-arm64
build-darwin-arm64:
	@mkdir -p $(DIST)/darwin-arm64
	GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/darwin-arm64/$(BINARY_CLI)    ./cmd/$(BINARY_CLI)/
	GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/darwin-arm64/$(BINARY_DAEMON) ./cmd/$(BINARY_DAEMON)/
	@echo "→ $(DIST)/darwin-arm64/"

.PHONY: build-windows
build-windows:
	@mkdir -p $(DIST)/windows-amd64
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/windows-amd64/$(BINARY_CLI).exe    ./cmd/$(BINARY_CLI)/
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" \
		-o $(DIST)/windows-amd64/$(BINARY_DAEMON).exe ./cmd/$(BINARY_DAEMON)/
	@echo "→ $(DIST)/windows-amd64/"

.PHONY: release
release: build-linux build-darwin build-darwin-arm64 build-windows
	@echo ""
	@echo "Release artefacts:"
	@find $(DIST) -type f | sort | sed 's/^/  /'

# ── quality ───────────────────────────────────────────────────────────────────

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 || \
		(echo "golangci-lint not found. Install: https://golangci-lint.run/usage/install/"; exit 1)
	golangci-lint run ./...

# ── clean ─────────────────────────────────────────────────────────────────────

.PHONY: clean
clean:
	rm -f $(BINARY_CLI) $(BINARY_DAEMON)
	rm -f $(BINARY_CLI).exe $(BINARY_DAEMON).exe
	rm -rf $(DIST)
	@echo "Cleaned."
