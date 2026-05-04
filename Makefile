.PHONY: help build clean-build notarize-build test-ginkgo test-watch coverage clean staticcheck deadcode audit clyde-check \
        install-build-guard uninstall-build-guard setup-hooks \
        deploy install install-system-agent install-launch-agent install-launch-agent-files uninstall-launch-agent \
        install-systemd-user install-systemd-user-files uninstall-systemd-user install-hook uninstall-hook \
        release-local release-snapshot sign notarize dist/clyde

# Optional local overrides (signing creds, never committed). Copy config.mk.example.
-include config.mk

# ---------------------------------------------------------------------------
# go-makefile bootstrap (https://github.com/agoodkind/go-makefile)
#
# Fetches go.mk at runtime into .make/go.mk (cached at ~/.cache/go-makefile)
# and -includes it. The included go.mk owns: lint, lint-tools, lint-golangci,
# lint-format, lint-gocyclo, fmt, vet, govulncheck, staticcheck-extra*,
# update-go-mk, go-mk-sync. Run `make update-go-mk` to refresh.
# ---------------------------------------------------------------------------

GO_MK_URL     := https://raw.githubusercontent.com/agoodkind/go-makefile/main/go.mk
GO_MK_API_URL := https://api.github.com/repos/agoodkind/go-makefile/contents/go.mk?ref=main
GO_MK         := .make/go.mk
GO_MK_CACHE   := $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/go-makefile/go.mk
# Dev override: when GO_MK_DEV_DIR points at a local go-makefile checkout
# (e.g. GO_MK_DEV_DIR=$$HOME/Sites/go-makefile), copy $(GO_MK_DEV_DIR)/go.mk
# verbatim and bypass the curl + cache write. Lets you iterate on go.mk
# without committing/pushing first.
GO_MK_DEV_DIR ?=

# Use go.mk's built-in golangci-lint baseline gate.
GOLANGCI_LINT_BASELINE ?= .golangci-lint-baseline.txt

# Define CMD so go.mk's default `build:` rule is suppressed (project owns build).
CMD := ./cmd/clyde

# Enable every bundled staticcheck-extra analyzer (5 core + 12 strict).
STATICCHECK_EXTRA_FLAGS         = $(STATICCHECK_EXTRA_CORE_FLAGS) $(STATICCHECK_EXTRA_STRICT_FLAGS)
STATICCHECK_EXTRA_EXCLUDE_PATHS = \.pb\.go:,/api/

# Attempts to refresh go.mk before every Makefile parse, then falls back to the
# cached copy when the network or upstream is unavailable.
GO_MK_BOOTSTRAP := $(shell \
	mkdir -p "$(dir $(GO_MK))" "$(dir $(GO_MK_CACHE))"; \
	if [ -n "$(GO_MK_DEV_DIR)" ] && [ -f "$(GO_MK_DEV_DIR)/go.mk" ]; then \
		cp "$(GO_MK_DEV_DIR)/go.mk" "$(GO_MK)"; \
		printf '%s\n' "go.mk: using dev override $(GO_MK_DEV_DIR)/go.mk" >&2; \
	else \
		tmp="$(GO_MK).tmp"; \
		if curl -fsSL -H "Accept: application/vnd.github.raw" --connect-timeout 5 --max-time 10 "$(GO_MK_API_URL)" -o "$$tmp" || curl -fsSL --connect-timeout 5 --max-time 10 "$(GO_MK_URL)?v=$$(date +%s)" -o "$$tmp" || curl -fsSL --connect-timeout 5 --max-time 10 "$(GO_MK_URL)" -o "$$tmp"; then \
			mv "$$tmp" "$(GO_MK)"; \
			cp "$(GO_MK)" "$(GO_MK_CACHE)"; \
		elif [ -f "$(GO_MK_CACHE)" ]; then \
			rm -f "$$tmp"; \
			cp "$(GO_MK_CACHE)" "$(GO_MK)"; \
		elif [ ! -f "$(GO_MK)" ]; then \
			rm -f "$$tmp"; \
			printf '%s\n' "error: go.mk fetch failed and no cache available" >&2; \
		fi; \
	fi)

$(GO_MK):
	@mkdir -p $(dir $@)
	@if [ -n "$(GO_MK_DEV_DIR)" ] && [ -f "$(GO_MK_DEV_DIR)/go.mk" ]; then \
		cp "$(GO_MK_DEV_DIR)/go.mk" "$@"; \
		echo "go.mk: using dev override $(GO_MK_DEV_DIR)/go.mk" >&2; \
	elif curl -fsSL -H "Accept: application/vnd.github.raw" --connect-timeout 5 --max-time 10 "$(GO_MK_API_URL)" -o "$@" || curl -fsSL --connect-timeout 5 --max-time 10 "$(GO_MK_URL)?v=$$(date +%s)" -o "$@" || curl -fsSL --connect-timeout 5 --max-time 10 "$(GO_MK_URL)" -o "$@"; then \
		mkdir -p "$(dir $(GO_MK_CACHE))" && cp "$@" "$(GO_MK_CACHE)"; \
	elif [ -f "$(GO_MK_CACHE)" ]; then \
		echo "warning: go.mk fetch failed, using cached version" >&2; \
		cp "$(GO_MK_CACHE)" "$@"; \
	else \
		echo "error: go.mk fetch failed and no cache available" >&2; \
		exit 1; \
	fi

-include $(GO_MK)

# ---------------------------------------------------------------------------
# Version / build metadata
# ---------------------------------------------------------------------------

BASE_VERSION := $(shell cat VERSION 2>/dev/null || echo "0.0.0")
GIT_TAG      := $(shell git describe --exact-match --tags 2>/dev/null)
COMMIT       := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_DIRTY    := $(shell git diff --quiet && echo false || echo true)
DATE         := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GKLOG_VPKG   := goodkind.io/gklog/version

# Use the exact git tag when present; otherwise stamp with -dev+timestamp.
ifeq ($(GIT_TAG),)
VERSION := $(BASE_VERSION)-dev+$(shell date -u +"%Y%m%d%H%M%S")
else
VERSION := $(patsubst v%,%,$(GIT_TAG))
endif

LDFLAGS := -X '$(GKLOG_VPKG).Commit=$(COMMIT)' \
           -X '$(GKLOG_VPKG).Dirty=$(GIT_DIRTY)' \
           -X '$(GKLOG_VPKG).BuildTime=$(DATE)' \
           -X '$(GKLOG_VPKG).BinHash='

GO_SRC := $(shell find . -name '*.go' -not -path './vendor/*')

CODESIGN_TIMESTAMP ?= none

define codesign_binary
	@if [ "$$(uname)" = "Darwin" ]; then \
		if [ -z "$(CODESIGN_IDENTITY)" ]; then \
			echo "No Developer ID Application signing identity found."; \
			echo "Set CERT_ID in config.mk or install a Developer ID Application certificate."; \
			exit 1; \
		fi; \
		echo "Signing $(1) with $(CODESIGN_IDENTITY)..."; \
		codesign --force --sign "$(CODESIGN_IDENTITY)" --identifier "$(BUNDLE_ID)" --options runtime --timestamp=$(CODESIGN_TIMESTAMP) "$(1)"; \
		codesign --verify --verbose=2 "$(1)"; \
	fi
endef

define reserve_dist_clyde
	@mkdir -p dist
	@chmod -R u+w dist/clyde 2>/dev/null; true
	@rm -rf dist/clyde
	@mkdir -p dist/clyde
	@printf '%s\n' \
		'Do not run binaries from this directory.' \
		'Use ~/.local/bin/clyde so macOS Full Disk Access has one stable client path.' \
		'Builds use temporary files and install only to ~/.local/bin/clyde.' \
		> dist/clyde/README.txt
	@chmod 0555 dist/clyde
endef

# ---------------------------------------------------------------------------
# Daemon install paths
# ---------------------------------------------------------------------------

LAUNCH_AGENT_LABEL    := io.goodkind.clyde.daemon
LAUNCH_AGENT_PLIST    := $(HOME)/Library/LaunchAgents/$(LAUNCH_AGENT_LABEL).plist
LAUNCH_AGENT_TEMPLATE := packaging/macos/io.goodkind.clyde.daemon.plist.in
DAEMON_LOG            := $(HOME)/Library/Logs/clyde-daemon.log
SYSTEMD_USER_SERVICE  := clyde-daemon.service
SYSTEMD_USER_DIR      := $(HOME)/.config/systemd/user
SYSTEMD_USER_UNIT     := $(SYSTEMD_USER_DIR)/$(SYSTEMD_USER_SERVICE)
SYSTEMD_USER_TEMPLATE := packaging/systemd/clyde-daemon.service.in
CLYDE_BIN             := $(HOME)/.local/bin/clyde
CLYDE_DAEMON_BIN      := $(CLYDE_BIN)
CLYDE_DEV_RUN         := $(CLYDE_BIN)
CLYDE_BUILD_DIR       := .make/build
CLYDE_BUILD_BIN       := $(CLYDE_BUILD_DIR)/clyde
NOTARIZE_ZIP          := dist/clyde-notarize.zip
UID                   := $(shell id -u)
BUNDLE_ID             ?= io.goodkind.clyde
CODESIGN_IDENTITY     := $(or $(CERT_ID),$(shell if [ "$$(uname)" = "Darwin" ]; then security find-identity -v -p codesigning 2>/dev/null | awk '/Developer ID Application/ { print $$2; exit }'; fi))
GO                     ?= go

.DEFAULT_GOAL := help

# ---------------------------------------------------------------------------
# Default
# ---------------------------------------------------------------------------

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST) | sort

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

build: build-check $(CLYDE_BUILD_BIN) dist/clyde ## Run non-test checks, then build and signing-check the clyde binary
	@echo "✓ Build and signing check passed"

$(CLYDE_BUILD_BIN): $(GO_SRC) go.mod go.sum
	@echo "Building clyde..."
	@mkdir -p "$(CLYDE_BUILD_DIR)"
	@tmp="$$(mktemp -t clyde-build.XXXXXX)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	go build -ldflags "$(LDFLAGS)" -o "$$tmp" ./cmd/clyde; \
	test -s "$$tmp"; \
	chmod 0755 "$$tmp"; \
	mv -f "$$tmp" "$(CLYDE_BUILD_BIN)"
	$(call codesign_binary,$(CLYDE_BUILD_BIN))

dist/clyde:
	$(call reserve_dist_clyde)
	@echo "✓ Reserved dist/clyde as a non-runtime directory"

clean-build: ## Remove the cached build artifact
	@rm -rf "$(CLYDE_BUILD_DIR)"

clean: ## Remove build artifacts
	@echo "Cleaning..."
	@chmod -R u+w dist 2>/dev/null; true
	@rm -rf dist/ "$(CLYDE_BUILD_DIR)"
	@rm -f *.test *.out coverage.txt coverage.html
	@find . -name "*.test" -delete
	$(call reserve_dist_clyde)
	@echo "✓ Cleaned"

# ---------------------------------------------------------------------------
# Test (go.mk owns `test`; this target preserves the previous Ginkgo runner)
# ---------------------------------------------------------------------------

test-ginkgo: ## Run tests with Ginkgo
	@go run github.com/onsi/ginkgo/v2/ginkgo -r --randomize-all --randomize-suites --fail-on-pending --race

ci-test: test-ginkgo ## Run the project CI test suite

test-watch: ## Run tests in watch mode
	@echo "Starting test watch mode..."
	@go run github.com/onsi/ginkgo/v2/ginkgo watch -r

coverage: ## Generate coverage report
	@echo "Generating coverage report..."
	@go run github.com/onsi/ginkgo/v2/ginkgo -r --randomize-all --randomize-suites --cover --coverprofile=coverage.txt
	@go tool cover -html=coverage.txt -o coverage.html
	@echo "✓ Coverage report generated: coverage.html"

# ---------------------------------------------------------------------------
# Code quality
#
# `lint`, `build-check`, `lint-golangci-baseline`, `staticcheck-extra*`,
# `fmt`, `vet`, and `govulncheck` are owned by go.mk. The project layers
# Clyde-specific analyzer targets on top without reimplementing those gates.
# ---------------------------------------------------------------------------

check: clyde-check ## Run go.mk's full gate plus Clyde-specific analyzers

build-check: clyde-check ## Run go.mk's non-test gate plus Clyde-specific analyzers

clyde-check: staticcheck deadcode audit ## Run Clyde-specific analyzer targets

staticcheck: ## Run Clyde's staticcheck bundle and custom architecture analyzers
	@echo "Running Clyde staticcheck..."
	@bash -c '\
		out=$$(go tool clyde-staticcheck ./... 2>&1 || true); \
		filtered=$$(printf "%s\n" "$$out" \
			| grep -Ev "\\.pb\\.go:|/api/" \
			| grep -Ev "^go: error obtaining buildID" \
			|| true); \
		if [ -n "$$filtered" ]; then \
			printf "%s\n" "$$filtered"; \
			exit 1; \
		fi; \
		echo "✓ Staticcheck passed"'

deadcode: ## Check for unreachable functions
	@if ! output=$$(go tool deadcode ./...); then \
		echo "go tool deadcode failed"; \
		exit 1; \
	fi; \
	filtered=$$(echo "$$output" | grep -v \
		-e 'cmd/root.go:.*NewRootCmd' \
		-e 'internal/mitm/\(baseline_paths\|capture_session\|codegen\|codegen_v2\|drift_runner\|launch_profile\|launcher\).go:' \
		-e 'internal/testutil/claude.go:.*CreateFakeClaude' \
		-e 'internal/testutil/claude.go:.*ReadClaudeArgs' \
	|| true); \
	if [ -n "$$filtered" ]; then \
		echo "Dead code found:"; \
		echo "$$filtered"; \
		exit 1; \
	fi
	@echo "✓ No dead code found"

audit: lint-gocyclo govulncheck ## Run Clyde audit checks via go.mk primitives

release-local: ## Run a full GoReleaser release with 1Password-backed Apple notarization
	@[ -f notarize.env ] || { echo "notarize.env not found. Copy notarize.env.example and fill in your 1Password op:// paths."; exit 1; }
	@GOFLAGS= op run --env-file=notarize.env -- goreleaser release --clean

release-snapshot: ## Build release artifacts locally without publishing or notarizing
	@GOFLAGS= goreleaser release --snapshot --clean --skip=publish --skip=notarize

install: build ## Install the signed clyde binary to ~/.local/bin/clyde
	@mkdir -p "$(dir $(CLYDE_BIN))"
	@out="$$(mktemp "$(CLYDE_BIN).new.XXXXXX")"; \
	trap 'rm -f "$$out"' EXIT; \
	cp -f "$(CLYDE_BUILD_BIN)" "$$out"; \
	chmod 0755 "$$out"; \
	test -s "$$out"; \
	mv -f "$$out" "$(CLYDE_BIN)"
	@echo "✓ Installed $(CLYDE_BIN)"

install-build-guard: ## Enforce repo staticcheck on direct go build via GOFLAGS toolexec
	@./scripts/install-go-build-guard.sh

uninstall-build-guard: ## Remove the repo staticcheck toolexec from GOFLAGS
	@./scripts/install-go-build-guard.sh --uninstall

# ---------------------------------------------------------------------------
# Install / deploy daemon
# ---------------------------------------------------------------------------

setup-hooks: ## Configure git hooks
	@git config core.hooksPath .githooks
	@chmod +x .githooks/*
	@echo "✓ Git hooks configured"

deploy: install ## Install binary and reload the daemon, installing the platform agent if needed
	@"$(CLYDE_BIN)" daemon reload || $(MAKE) install-system-agent

install-system-agent: ## Install/start the platform user agent (LaunchAgent on macOS, systemd user unit on Linux)
	@if [ "$$(uname)" = "Darwin" ]; then \
		$(MAKE) install-launch-agent; \
	elif command -v systemctl >/dev/null 2>&1; then \
		$(MAKE) install-systemd-user; \
	else \
		echo "install-system-agent needs launchctl on macOS or systemctl on Linux"; \
		exit 1; \
	fi

install-launch-agent: install-launch-agent-files ## Install/start the daemon LaunchAgent
	@launchctl bootout gui/$(UID) "$(LAUNCH_AGENT_PLIST)" 2>/dev/null; true
	@launchctl bootstrap gui/$(UID) "$(LAUNCH_AGENT_PLIST)"
	@echo "✓ LaunchAgent installed: $(LAUNCH_AGENT_PLIST)"
	@echo "  Logs: $(DAEMON_LOG)"

install-launch-agent-files: ## Render the daemon LaunchAgent plist
	@mkdir -p "$(HOME)/Library/LaunchAgents" "$(HOME)/Library/Logs"
	@touch "$(DAEMON_LOG)"
	@sed -e 's|@@CLYDE_DAEMON_BIN@@|$(CLYDE_DAEMON_BIN)|g' \
	     -e 's|@@HOME@@|$(HOME)|g' \
	     -e 's|@@LOG_PATH@@|$(DAEMON_LOG)|g' \
	     "$(LAUNCH_AGENT_TEMPLATE)" > "$(LAUNCH_AGENT_PLIST)"

uninstall-launch-agent: ## Remove the clyde daemon LaunchAgent
	@launchctl bootout gui/$(UID) "$(LAUNCH_AGENT_PLIST)" 2>/dev/null; true
	@rm -f "$(LAUNCH_AGENT_PLIST)"
	@echo "✓ LaunchAgent removed"

install-systemd-user: install-systemd-user-files ## Install/start the daemon systemd user unit
	@command -v systemctl >/dev/null 2>&1 || { echo "systemctl not found"; exit 1; }
	@systemctl --user daemon-reload
	@systemctl --user enable "$(SYSTEMD_USER_SERVICE)"
	@systemctl --user restart "$(SYSTEMD_USER_SERVICE)"
	@echo "✓ systemd user unit installed: $(SYSTEMD_USER_UNIT)"
	@echo "  Logs: journalctl --user -u $(SYSTEMD_USER_SERVICE) -f"

install-systemd-user-files: ## Render the daemon systemd user unit
	@mkdir -p "$(SYSTEMD_USER_DIR)"
	@sed -e 's|@@CLYDE_DAEMON_BIN@@|$(CLYDE_DAEMON_BIN)|g' \
	     -e 's|@@HOME@@|$(HOME)|g' \
	     "$(SYSTEMD_USER_TEMPLATE)" > "$(SYSTEMD_USER_UNIT)"

uninstall-systemd-user: ## Remove the clyde daemon systemd user unit
	@command -v systemctl >/dev/null 2>&1 || { echo "systemctl not found"; exit 1; }
	@systemctl --user disable --now "$(SYSTEMD_USER_SERVICE)" 2>/dev/null; true
	@rm -f "$(SYSTEMD_USER_UNIT)"
	@systemctl --user daemon-reload
	@echo "✓ systemd user unit removed"

install-hook: ## Register the SessionStart hook in ~/.claude/settings.json
	@mkdir -p "$(HOME)/.claude"
	@touch "$(HOME)/.claude/settings.json"
	@if [ ! -s "$(HOME)/.claude/settings.json" ]; then echo '{}' > "$(HOME)/.claude/settings.json"; fi
	@cp "$(HOME)/.claude/settings.json" "$(HOME)/.claude/settings.json.bak.$$(date +%s)"
	@jq --arg cmd "$(CLYDE_BIN) hook sessionstart" \
		'.hooks = (.hooks // {}) | .hooks.SessionStart = (.hooks.SessionStart // []) | \
		 .hooks.SessionStart = (.hooks.SessionStart | map(select(.hooks[0].command != $$cmd))) + \
		 [{matcher: "*", hooks: [{type: "command", command: $$cmd}]}]' \
		"$(HOME)/.claude/settings.json" > "$(HOME)/.claude/settings.json.tmp"
	@mv "$(HOME)/.claude/settings.json.tmp" "$(HOME)/.claude/settings.json"
	@echo "✓ SessionStart hook registered in ~/.claude/settings.json"

uninstall-hook: ## Remove the SessionStart hook from ~/.claude/settings.json
	@if [ -f "$(HOME)/.claude/settings.json" ]; then \
		jq --arg cmd "$(CLYDE_BIN) hook sessionstart" \
			'if .hooks.SessionStart then .hooks.SessionStart |= map(select(.hooks[0].command != $$cmd)) else . end' \
			"$(HOME)/.claude/settings.json" > "$(HOME)/.claude/settings.json.tmp" && \
		mv "$(HOME)/.claude/settings.json.tmp" "$(HOME)/.claude/settings.json"; \
		echo "✓ SessionStart hook removed"; \
	fi

# ---------------------------------------------------------------------------
# Distribution (macOS signing)
# ---------------------------------------------------------------------------

sign: build ## Build and signing-check binary with Developer ID Application certificate

notarize: CODESIGN_TIMESTAMP = timestamp
notarize: notarize-build dist/clyde ## Sign and notarize binary for distribution (requires NOTARY_PROFILE in config.mk)
	@echo "Creating notarization zip..."
	@rm -f "$(NOTARIZE_ZIP)"
	@ditto -c -k --keepParent "$(CLYDE_BUILD_BIN)" "$(NOTARIZE_ZIP)"
	@echo "Submitting for notarization (waiting)..."
	@xcrun notarytool submit "$(NOTARIZE_ZIP)" \
		--keychain-profile "$(NOTARY_PROFILE)" \
		--wait
	@echo "✓ Notarized $(CLYDE_BUILD_BIN)"

notarize-build: CODESIGN_TIMESTAMP = timestamp
notarize-build: clean-build $(CLYDE_BUILD_BIN)
