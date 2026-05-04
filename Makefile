.PHONY: help build test test-watch coverage clean fmt lint staticcheck staticcheck-extra staticcheck-extra-baseline staticcheck-extra-bin deadcode govulncheck audit \
        install-build-guard uninstall-build-guard setup-hooks \
        deploy install-launch-agent uninstall-launch-agent install-systemd-user uninstall-systemd-user install-hook uninstall-hook \
        release release-snapshot sign notarize dist/clyde

# Optional local overrides (signing creds, never committed). Copy config.mk.example.
-include config.mk

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
UID                   := $(shell id -u)
BUNDLE_ID             ?= io.goodkind.clyde
CODESIGN_IDENTITY     := $(or $(CERT_ID),$(shell if [ "$$(uname)" = "Darwin" ]; then security find-identity -v -p codesigning 2>/dev/null | awk '/Developer ID Application/ { print $$2; exit }'; fi))
GO                     ?= go

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

build: check dist/clyde ## Build and signing-check the clyde binary without leaving a repo-local executable
	@echo "Building clyde..."
	@tmp="$$(mktemp -t clyde-build.XXXXXX)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	go build -ldflags "$(LDFLAGS)" -o "$$tmp" ./cmd/clyde; \
	if [ "$$(uname)" = "Darwin" ]; then \
		if [ -z "$(CODESIGN_IDENTITY)" ]; then \
			echo "No Developer ID Application signing identity found."; \
			echo "Set CERT_ID in config.mk or install a Developer ID Application certificate."; \
			exit 1; \
		fi; \
		echo "Signing temporary clyde build with $(CODESIGN_IDENTITY)..."; \
		codesign --force --sign "$(CODESIGN_IDENTITY)" --identifier "$(BUNDLE_ID)" --options runtime --timestamp=none "$$tmp"; \
		codesign --verify --verbose=2 "$$tmp"; \
	fi
	@echo "✓ Build and signing check passed"

dist/clyde:
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
	@echo "✓ Reserved dist/clyde as a non-runtime directory"

clean: ## Remove build artifacts
	@echo "Cleaning..."
	@chmod -R u+w dist 2>/dev/null; true
	@rm -rf dist/
	@rm -f *.test *.out coverage.txt coverage.html
	@find . -name "*.test" -delete
	@mkdir -p dist/clyde
	@printf '%s\n' \
		'Do not run binaries from this directory.' \
		'Use ~/.local/bin/clyde so macOS Full Disk Access has one stable client path.' \
		'Builds use temporary files and install only to ~/.local/bin/clyde.' \
		> dist/clyde/README.txt
	@chmod 0555 dist/clyde
	@echo "✓ Cleaned"

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

test: ## Run tests with Ginkgo
	@go run github.com/onsi/ginkgo/v2/ginkgo -r --randomize-all --randomize-suites --fail-on-pending --race

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
# ---------------------------------------------------------------------------

check: lint staticcheck staticcheck-extra deadcode govulncheck audit

fmt: ## Format code with gofumpt and goimports
	@echo "Formatting code..."
	@go tool golangci-lint fmt ./...
	@echo "✓ Formatted"

lint: ## Run golangci-lint
	@echo "Running linter..."
	@go tool golangci-lint run ./... && echo "✓ Lint passed"

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

STATICCHECK_EXTRA_BIN           ?=
STATICCHECK_EXTRA_BUILD_REPO    ?=
STATICCHECK_EXTRA_BUILD_PKG     ?=
STATICCHECK_EXTRA_INSTALL       ?= github.com/agoodkind/go-makefile/staticcheck/cmd/staticcheck-extra@latest
STATICCHECK_EXTRA_FLAGS         ?= \
	-slog_error_without_err \
	-banned_direct_output \
	-hot_loop_info_log \
	-missing_boundary_log \
	-no_any_or_empty_interface \
	-wrapped_error_without_slog \
	-os_exit_outside_main \
	-context_todo_in_production \
	-time_sleep_in_production \
	-panic_in_production \
	-time_now_outside_clock \
	-goroutine_without_recover \
	-silent_defer_close \
	-slog_missing_trace_id \
	-sensitive_field_in_log \
	-grpc_handler_missing_peer_enrichment
STATICCHECK_EXTRA_TARGETS       ?= ./...
STATICCHECK_EXTRA_BASELINE      ?= .staticcheck-extra-baseline.txt
STATICCHECK_EXTRA_EXCLUDE_PATHS ?= \.pb\.go:|/api/

staticcheck-extra-bin:
	@bash -eu -c '\
		bin="$(STATICCHECK_EXTRA_BIN)"; \
		repo="$(STATICCHECK_EXTRA_BUILD_REPO)"; \
		pkg="$(STATICCHECK_EXTRA_BUILD_PKG)"; \
		install="$(STATICCHECK_EXTRA_INSTALL)"; \
		if [ -n "$$bin" ]; then \
			[ -x "$$bin" ] || { echo "staticcheck-extra: $$bin not executable"; exit 1; }; \
			exit 0; \
		fi; \
		if [ -n "$$repo" ]; then \
			if [ ! -d "$$repo" ]; then \
				echo "staticcheck-extra: build repo $$repo not present; skipping"; exit 0; \
			fi; \
			if [ -z "$$pkg" ]; then \
				echo "staticcheck-extra: STATICCHECK_EXTRA_BUILD_PKG not set"; exit 1; \
			fi; \
			mkdir -p .make; \
			out="$(CURDIR)/.make/staticcheck-extra"; \
			newest_src=$$(find "$$repo" -name "*.go" -newer "$$out" 2>/dev/null | head -1 || true); \
			if [ ! -x "$$out" ] || [ -n "$$newest_src" ]; then \
				cd "$$repo" && $(GO) build -o "$$out" "$$pkg"; \
			fi; \
			exit 0; \
		fi; \
		if [ -z "$$install" ]; then exit 0; fi; \
		mkdir -p .make; \
		out="$(CURDIR)/.make/staticcheck-extra"; \
		base=$$(basename "$$install" | sed "s/@.*//"); \
		gobin=$$($(GO) env GOPATH)/bin; \
		installed="$$gobin/$$base"; \
		if [ ! -x "$$installed" ]; then \
			GOBIN="$$gobin" $(GO) install "$$install"; \
		fi; \
		ln -sf "$$installed" "$$out"'

staticcheck-extra: staticcheck-extra-bin
	@bash -eu -o pipefail -c '\
		bin="$(STATICCHECK_EXTRA_BIN)"; \
		[ -z "$$bin" ] && [ -x .make/staticcheck-extra ] && bin=".make/staticcheck-extra"; \
		if [ -z "$$bin" ]; then \
			echo "staticcheck-extra: not configured (skipped)"; exit 0; \
		fi; \
		if [ ! -x "$$bin" ]; then \
			echo "staticcheck-extra: binary $$bin not executable; skipping"; exit 0; \
		fi; \
		mkdir -p .make; \
		excludes="$(STATICCHECK_EXTRA_EXCLUDE_PATHS)"; \
		filter() { \
			if [ -z "$$excludes" ]; then cat; return; fi; \
			grep -Ev "$$excludes" || true; \
		}; \
		"$$bin" $(STATICCHECK_EXTRA_FLAGS) $(STATICCHECK_EXTRA_TARGETS) 2>&1 \
			| sed "s|$(CURDIR)/||g" | filter | sort > .make/staticcheck-extra.out || true; \
		if [ ! -f "$(STATICCHECK_EXTRA_BASELINE)" ]; then \
			touch "$(STATICCHECK_EXTRA_BASELINE)"; \
		fi; \
		new=$$(comm -23 .make/staticcheck-extra.out "$(STATICCHECK_EXTRA_BASELINE)" || true); \
		if [ -n "$$new" ]; then \
			echo "NEW staticcheck-extra findings (not in baseline):"; \
			echo "$$new"; \
			echo ""; \
			echo "Either fix them, or refresh the baseline:"; \
			echo "  make staticcheck-extra-baseline"; \
			exit 1; \
		fi; \
		gone=$$(comm -13 .make/staticcheck-extra.out "$(STATICCHECK_EXTRA_BASELINE)" || true); \
		if [ -n "$$gone" ]; then \
			echo "RESOLVED staticcheck-extra findings (please refresh baseline):"; \
			echo "$$gone"; \
		fi; \
		n=$$(wc -l < .make/staticcheck-extra.out); \
		echo "staticcheck-extra: OK ($$n findings, all in baseline)"'

staticcheck-extra-baseline: staticcheck-extra-bin
	@bash -eu -o pipefail -c '\
		bin="$(STATICCHECK_EXTRA_BIN)"; \
		[ -z "$$bin" ] && [ -x .make/staticcheck-extra ] && bin=".make/staticcheck-extra"; \
		if [ -z "$$bin" ] || [ ! -x "$$bin" ]; then \
			echo "staticcheck-extra: not configured; cannot refresh baseline"; exit 1; \
		fi; \
		excludes="$(STATICCHECK_EXTRA_EXCLUDE_PATHS)"; \
		filter() { \
			if [ -z "$$excludes" ]; then cat; return; fi; \
			grep -Ev "$$excludes" || true; \
		}; \
		"$$bin" $(STATICCHECK_EXTRA_FLAGS) $(STATICCHECK_EXTRA_TARGETS) 2>&1 \
			| sed "s|$(CURDIR)/||g" | filter | sort > "$(STATICCHECK_EXTRA_BASELINE)" || true; \
		n=$$(wc -l < "$(STATICCHECK_EXTRA_BASELINE)"); \
		echo "staticcheck-extra: baseline $(STATICCHECK_EXTRA_BASELINE) refreshed ($$n findings)"'

deadcode: build ## Check for unreachable functions
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

govulncheck: ## Run vulnerability check
	@go tool govulncheck ./...

audit: ## Run complexity and vulnerability checks
	@echo "=== Cyclomatic complexity (>40) ==="
	@go tool gocyclo -over 40 .
	@echo ""
	@echo "=== Vulnerability check ==="
	@go tool govulncheck ./...

release: ## Run a full GoReleaser release with 1Password-backed Apple notarization
	@[ -f notarize.env ] || { echo "notarize.env not found. Copy notarize.env.example and fill in your 1Password op:// paths."; exit 1; }
	@GOFLAGS= op run --env-file=notarize.env -- goreleaser release --clean

release-snapshot: ## Build release artifacts locally without publishing or notarizing
	@GOFLAGS= goreleaser release --snapshot --clean --skip=publish --skip=notarize

install: build ## Install the signed clyde binary to ~/.local/bin/clyde
	@mkdir -p "$(HOME)/.local/bin"
	@tmp="$$(mktemp -t clyde-install.XXXXXX)"; \
	out="$(CLYDE_BIN).new.$$"; \
	trap 'rm -f "$$tmp" "$$out"' EXIT; \
	set -e; \
	go build -ldflags "$(LDFLAGS)" -o "$$tmp" ./cmd/clyde; \
	test -s "$$tmp"; \
	chmod 0755 "$$tmp"; \
	if [ "$$(uname)" = "Darwin" ]; then \
		if [ -z "$(CODESIGN_IDENTITY)" ]; then \
			echo "No Developer ID Application signing identity found."; \
			echo "Set CERT_ID in config.mk or install a Developer ID Application certificate."; \
			exit 1; \
		fi; \
		echo "Signing install build with $(CODESIGN_IDENTITY)..."; \
		codesign --force --sign "$(CODESIGN_IDENTITY)" --identifier "$(BUNDLE_ID)" --options runtime --timestamp=none "$$tmp"; \
		codesign --verify --verbose=2 "$$tmp"; \
	fi; \
	cp -f "$$tmp" "$$out"; \
	chmod 0755 "$$out"; \
	test -s "$$out"; \
	mv -f "$$out" "$(CLYDE_BIN)"
	@if [ "$$(uname)" = "Darwin" ]; then codesign --verify --verbose=2 "$(CLYDE_BIN)"; fi
	@chmod -R u+w dist/clyde 2>/dev/null; true
	@rm -rf dist/clyde
	@mkdir -p dist/clyde
	@printf '%s\n' \
		'Do not run binaries from this directory.' \
		'Use ~/.local/bin/clyde so macOS Full Disk Access has one stable client path.' \
		'Builds use temporary files and install only to ~/.local/bin/clyde.' \
		> dist/clyde/README.txt
	@chmod 0555 dist/clyde
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

deploy: ## Install/start the daemon if needed; otherwise hand it off to the new binary
	@if [ "$$(uname)" = "Darwin" ]; then \
		if [ ! -f "$(LAUNCH_AGENT_PLIST)" ] || ! launchctl list "$(LAUNCH_AGENT_LABEL)" >/dev/null 2>&1; then \
			$(MAKE) install-launch-agent; \
		else \
			$(MAKE) install; \
			"$(CLYDE_BIN)" daemon reload || $(MAKE) install-launch-agent; \
		fi; \
	elif command -v systemctl >/dev/null 2>&1; then \
		if [ ! -f "$(SYSTEMD_USER_UNIT)" ] || ! systemctl --user is-active --quiet "$(SYSTEMD_USER_SERVICE)"; then \
			$(MAKE) install-systemd-user; \
		else \
			$(MAKE) install; \
			"$(CLYDE_BIN)" daemon reload; \
		fi; \
	else \
		echo "deploy needs launchctl on macOS or systemctl on Linux"; \
		exit 1; \
	fi

install-launch-agent: install ## Render and install the daemon LaunchAgent (runs OAuth refresh + adapter + prune in-process)
	@mkdir -p "$(HOME)/Library/LaunchAgents" "$(HOME)/Library/Logs"
	@touch "$(DAEMON_LOG)"
	@sed -e 's|@@CLYDE_DAEMON_BIN@@|$(CLYDE_DAEMON_BIN)|g' \
	     -e 's|@@HOME@@|$(HOME)|g' \
	     -e 's|@@LOG_PATH@@|$(DAEMON_LOG)|g' \
	     "$(LAUNCH_AGENT_TEMPLATE)" > "$(LAUNCH_AGENT_PLIST)"
	@launchctl bootout gui/$(UID) "$(LAUNCH_AGENT_PLIST)" 2>/dev/null; true
	@launchctl bootstrap gui/$(UID) "$(LAUNCH_AGENT_PLIST)"
	@echo "✓ LaunchAgent installed: $(LAUNCH_AGENT_PLIST)"
	@echo "  Logs: $(DAEMON_LOG)"

uninstall-launch-agent: ## Remove the clyde daemon LaunchAgent
	@launchctl bootout gui/$(UID) "$(LAUNCH_AGENT_PLIST)" 2>/dev/null; true
	@rm -f "$(LAUNCH_AGENT_PLIST)"
	@echo "✓ LaunchAgent removed"

install-systemd-user: install ## Render and install the daemon systemd user unit
	@command -v systemctl >/dev/null 2>&1 || { echo "systemctl not found"; exit 1; }
	@mkdir -p "$(SYSTEMD_USER_DIR)"
	@sed -e 's|@@CLYDE_DAEMON_BIN@@|$(CLYDE_DAEMON_BIN)|g' \
	     -e 's|@@HOME@@|$(HOME)|g' \
	     "$(SYSTEMD_USER_TEMPLATE)" > "$(SYSTEMD_USER_UNIT)"
	@systemctl --user daemon-reload
	@systemctl --user enable "$(SYSTEMD_USER_SERVICE)"
	@systemctl --user restart "$(SYSTEMD_USER_SERVICE)"
	@echo "✓ systemd user unit installed: $(SYSTEMD_USER_UNIT)"
	@echo "  Logs: journalctl --user -u $(SYSTEMD_USER_SERVICE) -f"

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

ifdef CERT_ID
sign: build ## Build and signing-check binary with Developer ID Application certificate

notarize: dist/clyde ## Sign and notarize binary for distribution (requires NOTARY_PROFILE in config.mk)
	@echo "Building signed notarization artifact..."
	@tmpdir="$$(mktemp -d -t clyde-notarize.XXXXXX)"; \
	trap 'rm -rf "$$tmpdir" dist/clyde-notarize.zip' EXIT; \
	go build -ldflags "$(LDFLAGS)" -o "$$tmpdir/clyde" ./cmd/clyde; \
	codesign --force --sign "$(CERT_ID)" --identifier "$(BUNDLE_ID)" --options runtime --timestamp "$$tmpdir/clyde"; \
	codesign --verify --verbose=2 "$$tmpdir/clyde"; \
	echo "Creating notarization zip..."; \
	ditto -c -k --keepParent "$$tmpdir/clyde" dist/clyde-notarize.zip; \
	echo "Submitting for notarization (waiting)..."; \
	xcrun notarytool submit dist/clyde-notarize.zip \
		--keychain-profile "$(NOTARY_PROFILE)" \
		--wait
	@echo "✓ Notarized dist/clyde"
else
sign: build ## Sign binary (requires CERT_ID in config.mk)
	@echo "⚠ CERT_ID not set in config.mk. Skipping code signing."
	@echo "  Copy config.mk.example to config.mk and fill in your Developer ID"

notarize: sign ## Sign and notarize binary (requires config.mk)
	@echo "⚠ CERT_ID not set in config.mk. Skipping notarization."
endif
