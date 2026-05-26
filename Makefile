GO          := mise exec -- go
BIN_DIR     := bin
INSTALL_DIR := $(HOME)/.local/bin

.PHONY: default build build-linux build-linux-amd64 build-linux-arm64 install uninstall test vet fmt install-hooks run-server ui-dev ui-build clean help

default: build

build:
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/parley  ./cmd/parley
	$(GO) build -o $(BIN_DIR)/parleyd ./cmd/parleyd

build-linux: build-linux-amd64 build-linux-arm64

build-linux-amd64:
	@mkdir -p $(BIN_DIR)/linux-amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/linux-amd64/parley  ./cmd/parley
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/linux-amd64/parleyd ./cmd/parleyd

build-linux-arm64:
	@mkdir -p $(BIN_DIR)/linux-arm64
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/linux-arm64/parley  ./cmd/parley
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o $(BIN_DIR)/linux-arm64/parleyd ./cmd/parleyd

install: build
	@mkdir -p $(INSTALL_DIR)
	rm -f $(INSTALL_DIR)/parley  && cp $(BIN_DIR)/parley  $(INSTALL_DIR)/parley
	rm -f $(INSTALL_DIR)/parleyd && cp $(BIN_DIR)/parleyd $(INSTALL_DIR)/parleyd
	@echo "installed parley + parleyd to $(INSTALL_DIR)"

uninstall:
	rm -f $(INSTALL_DIR)/parley $(INSTALL_DIR)/parleyd
	@echo "removed parley + parleyd from $(INSTALL_DIR)"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

install-hooks:
	cp hooks/pre-commit $(shell git rev-parse --git-common-dir)/hooks/pre-commit
	@echo "pre-commit hook installed"

run-server: build
	PARLEY_ADDR=:18080 $(BIN_DIR)/parleyd

clean:
	rm -rf $(BIN_DIR)

ui-dev: ## Start the Vite dev server (requires parleyd running on :18080)
	cd ui && npm run dev

ui-build: ## Build the static UI to ui/dist/
	cd ui && npm run build

help:
	@printf "%s\n" \
	  "Makefile targets:" \
	  "  build        compile both binaries to ./$(BIN_DIR)/ (default)" \
	  "  build-linux  cross-compile linux amd64 + arm64 binaries" \
	  "  install      copy binaries to $(INSTALL_DIR)/" \
	  "  uninstall    remove binaries from $(INSTALL_DIR)/" \
	  "  test         run go test ./..." \
	  "  vet          run go vet ./..." \
	  "  fmt          run gofmt -w on all Go files" \
	  "  install-hooks install git pre-commit hook (gofmt + vet)" \
	  "  run-server   start parleyd on :18080" \
	  "  ui-dev       start Vite dev server (requires parleyd on :18080)" \
	  "  ui-build     build static UI to ui/dist/" \
	  "  clean        delete ./$(BIN_DIR)/"
