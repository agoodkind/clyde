# Lint is centralized in go-makefile. Do NOT define project-local lint,
# audit, fmt, vet, or staticcheck targets here. They duplicate the central
# pipeline and let agents bypass strict rules. Run `make help` for the
# canonical entry points (build/check/lint/fmt) and per-linter sub-targets
# (lint-golangci, lint-format, lint-gocyclo, lint-deadcode,
# staticcheck-extra). Refresh baselines via the matching *-baseline target.
#
# clyde Makefile.
# Build/lint/release/service pipeline lives in go-makefile and is fetched at
# runtime. Project-local additions are the staticcheck-extra exclude for
# generated protobuf code, the Go 1.27 compatible vulnerability scanner, and
# ginkgo as an alternate test runner.

# Optional local overrides (signing creds, never committed). Copy config.mk.example.
-include config.mk

# Identity. clyde has no own version package; it cross-stamps gklog/version.
BINARY     := clyde
CMD        := ./cmd/$(BINARY)
GKLOG_VPKG := goodkind.io/gklog/version

# Daemon identity. go-service.mk reads these at parse time, so they must
# be set BEFORE include bootstrap.mk.
LAUNCHD_LABEL := io.goodkind.clyde.daemon
SYSTEMD_UNIT  := clyde-daemon.service
LOG_PATH      := $(HOME)/Library/Logs/clyde-daemon.log
SUPERVISOR_FINGERPRINT := $(shell ./scripts/supervisor-fingerprint.sh)
GO_BUILD_LDFLAGS += -X goodkind.io/clyde/internal/daemonsupervisor.BuildFingerprint=$(SUPERVISOR_FINGERPRINT)

# Exclude protobuf-generated code under /api/ from staticcheck-extra.
STATICCHECK_EXTRA_EXCLUDE_PATHS = \.pb\.go:,/api/
export GOVULNCHECK_INSTALL := golang.org/x/vuln/cmd/govulncheck@v1.7.0

# Pipeline modules
GO_MK_MODULES := go-build.mk go-release.mk go-service.mk

# go.mk runs these as order-only prerequisites of every build, lint, vet, test,
# and govulncheck target. GO_MK_GENERATE generates the Swift tree-sitter parser
# in the pinned gksyntax submodule and places the pinned static embedding model
# files. GO_MK_WORKSPACE_USE materializes a gitignored go.work that routes that
# submodule into the build. The pinned gksyntax module zip omits the dart and
# swift grammar C sources, and gomoddirectives rejects a go.mod replace.
GO_MK_GENERATE := gksyntax-grammars staticembed-model
GO_MK_GENERATE_INPUTS := third_party/gksyntax internal/conversation/staticembed/model
GO_MK_GENERATE_OUTPUTS := \
	internal/conversation/staticembed/model/model.safetensors \
	internal/conversation/staticembed/model/tokenizer.json \
	third_party/gksyntax/treesitter/grammars/swift/upstream/src/parser.c \
	third_party/gksyntax/treesitter/grammars/swift/upstream/src/tree_sitter/parser.h \
	third_party/gksyntax/treesitter/grammars/swift/upstream/src/tree_sitter/array.h \
	third_party/gksyntax/treesitter/grammars/swift/upstream/src/tree_sitter/alloc.h
GO_MK_WORKSPACE_USE := . third_party/gksyntax

include bootstrap.mk

.DEFAULT_GOAL := check

# ---------------------------------------------------------------------------
# Project-local
# ---------------------------------------------------------------------------

BUNDLE_ID         ?= io.goodkind.clyde
CODESIGN_IDENTITY := $(or $(CERT_ID),$(shell if [ "$$(uname)" = "Darwin" ]; then security find-identity -v -p codesigning 2>/dev/null | awk '/Developer ID Application/ { print $$2; exit }'; fi))

.PHONY: test-ginkgo test-watch coverage live setup-hooks install-hooks \
        deploy daemon-reload deadcode proto gksyntax-grammars staticembed-model \
        native-prereqs embedded-search-bootstrap

# Tests via Ginkgo. go.mk's `test` target uses `go test ./...` which already
# runs ginkgo specs registered through RunSpecs. test-ginkgo is for when you
# want the ginkgo runner's flags (randomize, race, etc.) explicitly.
# --timeout sits under the CI job's timeout-minutes so a spec that blocks forever
# fails the suite with a full progress report instead of being SIGKILLed by the
# runner with no diagnostics. --poll-progress-after dumps the goroutine of any
# node still running after the interval, which surfaces a CI-only hang that does
# not reproduce locally.
test-ginkgo: ## Run tests with Ginkgo's runner
	@go run github.com/onsi/ginkgo/v2/ginkgo -r --randomize-all --randomize-suites --fail-on-pending --race \
		--timeout=8m --poll-progress-after=60s --poll-progress-interval=30s

test-watch: ## Run ginkgo in watch mode
	@go run github.com/onsi/ginkgo/v2/ginkgo watch -r

coverage: ## Generate coverage report via ginkgo
	@go run github.com/onsi/ginkgo/v2/ginkgo -r --randomize-all --randomize-suites --cover --coverprofile=coverage.txt
	@go tool cover -html=coverage.txt -o coverage.html
	@echo "coverage report: coverage.html"

# Live daemon validation. Boots the real daemon binary in isolated temp XDG roots
# on throwaway ports and drives its listeners; the default `go test ./...` never
# runs it (build tag `live`). To run several isolated instances at once, export a
# disjoint port set per run: CLYDE_TEST_ADAPTER_PORT, CLYDE_TEST_MITM_PORT,
# CLYDE_TEST_CURSOR_PORT, CLYDE_TEST_TOPOLOGY_PORT, CLYDE_TEST_MOVED_MITM_PORT,
# CLYDE_TEST_CONVERSATION_INGESTION, CLYDE_TEST_CONVERSATION_SEARCH,
# and CLYDE_TEST_COLLECTION_ID.
live: ## Run the live daemon validation suite (opt-in, build tag live)
	@go test -tags live -count=1 ./test/live/

deadcode: lint-deadcode ## Alias for the central deadcode gate

# ---------------------------------------------------------------------------
# gksyntax submodule grammars and embedded search native build
# ---------------------------------------------------------------------------
# Embedded conversation search imports goodkind.io/gksyntax/shelldecomp. The
# gksyntax repository vendors the dart and swift grammars as its own
# submodules, and it commits only the swift grammar definition. This recipe
# initializes the pinned recursive submodule, installs the tree-sitter CLI
# version that gksyntax pins into .bin, and generates the swift parser inside
# the submodule working tree. The nested dart grammar declares an SSH
# submodule URL, which the HTTPS rewrite replaces for hosts without SSH keys.
GKS_DIR := third_party/gksyntax
SWIFT_GRAMMAR_DIR := $(GKS_DIR)/treesitter/grammars/swift/upstream
SWIFT_GRAMMAR_DEF := $(SWIFT_GRAMMAR_DIR)/src/grammar.json
SWIFT_GRAMMAR_PARSER := $(SWIFT_GRAMMAR_DIR)/src/parser.c
TREE_SITTER_ABI := 14
TREE_SITTER_LOCAL_DIR := $(CURDIR)/.bin
TREE_SITTER_BIN := $(TREE_SITTER_LOCAL_DIR)/tree-sitter

gksyntax-grammars: ## Initialize the pinned gksyntax submodule and generate its Swift parser
	@set -e; \
	status="$$(git submodule status --recursive $(GKS_DIR))"; \
	if printf '%s\n' "$$status" | grep -q '^U'; then \
		echo "gksyntax-grammars: $(GKS_DIR) has unresolved submodule conflicts" >&2; \
		exit 1; \
	fi; \
	if printf '%s\n' "$$status" | grep -Eq '^[+-]'; then \
		git -c url.https://github.com/.insteadOf=git@github.com: submodule update --init --recursive $(GKS_DIR); \
	fi
	@if [ ! -f "$(SWIFT_GRAMMAR_DEF)" ]; then \
		echo "gksyntax-grammars: $(SWIFT_GRAMMAR_DEF) is missing after submodule initialization" >&2; \
		exit 1; \
	fi
	@"$(GKS_DIR)/scripts/install-tree-sitter.sh" "$(TREE_SITTER_LOCAL_DIR)"
	@set -e; \
	if [ ! -f "$(SWIFT_GRAMMAR_PARSER)" ] || [ "$(SWIFT_GRAMMAR_DEF)" -nt "$(SWIFT_GRAMMAR_PARSER)" ]; then \
		echo "gksyntax-grammars: generating Swift parser (abi $(TREE_SITTER_ABI))"; \
		( cd "$(SWIFT_GRAMMAR_DIR)" && "$(TREE_SITTER_BIN)" generate src/grammar.json --abi $(TREE_SITTER_ABI) ); \
		git -C "$(SWIFT_GRAMMAR_DIR)" checkout -- .; \
	else \
		echo "gksyntax-grammars: Swift parser already generated"; \
	fi

# staticembed-model places the model files that internal/conversation/staticembed
# compiles into the binary. model/manifest.json pins the revision and SHA-256 of
# each file. A file with the pinned hash stays; any other file downloads again.
staticembed-model: ## Fetch and verify the pinned static embedding model files
	GOWORK=off CGO_ENABLED=0 go run ./cmd/staticembed-model

# The formatters load packages, and go:embed fails to load staticembed without
# the model files. go.mk runs GO_MK_GENERATE before build, lint, vet, and test,
# but not before these two targets.
lint-format fmt: | staticembed-model

# native-prereqs runs the go.mk order-only prerequisites that `make test` runs
# before it compiles packages. CI jobs that call `go test` directly run it first.
native-prereqs: | $(GO_MK_PREREQS) ## Prepare the gksyntax grammars, go.work, and cgo dependencies

# EMBEDDED_SEARCH_PACKAGES lists the external packages that the embedded
# conversation search path imports.
EMBEDDED_SEARCH_PACKAGES := goodkind.io/gksyntax/shelldecomp

embedded-search-bootstrap: | $(GO_MK_PREREQS) ## Compile the embedded search native imports in the pinned workspace
	CGO_ENABLED=1 go build $(EMBEDDED_SEARCH_PACKAGES)

# ---------------------------------------------------------------------------
# Protobuf / gRPC codegen. Sources live under api/**/*.proto; config is
# buf.yaml + buf.gen.yaml (remote plugins, so only the buf binary is needed).
# Wired as a prerequisite of build so generated code stays in sync; note buf
# generate reaches buf.build for remote plugins, so it needs network.
# ---------------------------------------------------------------------------

proto: ## Regenerate protobuf/gRPC Go code from api/**/*.proto via buf
	@command -v buf >/dev/null 2>&1 || go install github.com/bufbuild/buf/cmd/buf@v1.70.0
	@PATH="$$(go env GOPATH)/bin:$$PATH" buf generate

build: proto

setup-hooks: ## Configure git hooks
	@git config core.hooksPath .githooks
	@chmod +x .githooks/*
	@echo "git hooks configured"

install-hooks: install ## Install user-scoped Clyde hooks
	@"$(INSTALL_BIN)" install hooks --clyde-bin "$(INSTALL_BIN)"

# ---------------------------------------------------------------------------
# Deploy: install canonical binary, ensure supervisor ownership, reload daemon.
# Clyde keeps `clyde daemon reload` as the binary-handoff path, but launchd or
# systemd must own normal deployed daemon startup before the reload RPC runs.
# ---------------------------------------------------------------------------

deploy: install ## Install binary, ensure supervisor ownership, reload daemon, and print service status
	@INSTALL_BIN="$(INSTALL_BIN)" \
		LAUNCHD_LABEL="$(LAUNCHD_LABEL)" \
		LAUNCHD_PLIST="$(LAUNCHD_PLIST)" \
		LAUNCHD_DOMAIN="$(LAUNCHD_DOMAIN)" \
		SYSTEMD_UNIT="$(SYSTEMD_UNIT)" \
		SYSTEMD_USER_UNIT="$(SYSTEMD_USER_UNIT)" \
		LOG_PATH="$(LOG_PATH)" \
		"$(INSTALL_BIN)" daemon deploy

daemon-reload: install ## Reload or restart the daemon without changing service config
	@INSTALL_BIN="$(INSTALL_BIN)" \
		LAUNCHD_LABEL="$(LAUNCHD_LABEL)" \
		LAUNCHD_PLIST="$(LAUNCHD_PLIST)" \
		LAUNCHD_DOMAIN="$(LAUNCHD_DOMAIN)" \
		SYSTEMD_UNIT="$(SYSTEMD_UNIT)" \
		SYSTEMD_USER_UNIT="$(SYSTEMD_USER_UNIT)" \
		LOG_PATH="$(LOG_PATH)" \
		"$(INSTALL_BIN)" daemon deploy --reload-only
