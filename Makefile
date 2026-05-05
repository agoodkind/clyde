# clyde Makefile.
# Build/lint/release/service pipeline lives in go-makefile and is fetched at
# runtime. Project-local: clyde-staticcheck custom analyzer, deadcode allowlist,
# ginkgo as an alternate test runner, build-guard hook, and the SessionStart
# hook installer.

GO_MK_URL     := https://raw.githubusercontent.com/agoodkind/go-makefile/main/go.mk
GO_MK_API_URL := https://api.github.com/repos/agoodkind/go-makefile/contents/go.mk?ref=main
GO_MK         := .make/go.mk
GO_MK_CACHE   := $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/go-makefile/go.mk
# Dev override: GO_MK_DEV_DIR=$HOME/Sites/go-makefile to iterate locally.
GO_MK_DEV_DIR ?=

# Optional local overrides (signing creds, never committed). Copy config.mk.example.
-include config.mk

# Identity. clyde has no own version package; it cross-stamps gklog/version.
BINARY     := clyde
CMD        := ./cmd/$(BINARY)
GKLOG_VPKG := goodkind.io/gklog/version

# Daemon identity. go-service.mk reads these at parse time, so they must
# be set BEFORE -include $(GO_MK).
LAUNCHD_LABEL := io.goodkind.clyde.daemon
SYSTEMD_UNIT  := clyde-daemon.service
LOG_PATH      := $(HOME)/Library/Logs/clyde-daemon.log

# Exclude protobuf-generated code under /api/ from staticcheck-extra.
# _test.go is already excluded by go.mk default.
STATICCHECK_EXTRA_EXCLUDE_PATHS = \.pb\.go:,/api/

# Project allowlist for the central lint-deadcode gate. cmd/root.go's
# NewRootCmd is the cobra entrypoint reached via reflection. The mitm/
# scenarios are picked up by a runtime registry. testutil/claude exposes
# fake helpers used only by package-external tests, which deadcode does
# not load. Kept on one line to avoid Make line-continuation whitespace
# bleeding into the regex.
DEADCODE_EXCLUDE_PATHS = cmd/root.go:.*NewRootCmd,internal/mitm/(baseline_paths|capture_session|codegen|codegen_v2|drift_runner|launch_profile|launcher)\.go:,internal/testutil/claude.go:.*CreateFakeClaude,internal/testutil/claude.go:.*ReadClaudeArgs

# Pipeline modules
GO_MK_MODULES := go-build.mk go-release.mk go-service.mk

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

.DEFAULT_GOAL := check

# ---------------------------------------------------------------------------
# Project-local
# ---------------------------------------------------------------------------

BUNDLE_ID         ?= io.goodkind.clyde
CODESIGN_IDENTITY := $(or $(CERT_ID),$(shell if [ "$$(uname)" = "Darwin" ]; then security find-identity -v -p codesigning 2>/dev/null | awk '/Developer ID Application/ { print $$2; exit }'; fi))

.PHONY: test-ginkgo test-watch coverage \
        install-build-guard uninstall-build-guard setup-hooks \
        install-hook uninstall-hook \
        deploy

# Tests via Ginkgo. go.mk's `test` target uses `go test ./...` which already
# runs ginkgo specs registered through RunSpecs. test-ginkgo is for when you
# want the ginkgo runner's flags (randomize, race, etc.) explicitly.
test-ginkgo: ## Run tests with Ginkgo's runner
	@go run github.com/onsi/ginkgo/v2/ginkgo -r --randomize-all --randomize-suites --fail-on-pending --race

test-watch: ## Run ginkgo in watch mode
	@go run github.com/onsi/ginkgo/v2/ginkgo watch -r

coverage: ## Generate coverage report via ginkgo
	@go run github.com/onsi/ginkgo/v2/ginkgo -r --randomize-all --randomize-suites --cover --coverprofile=coverage.txt
	@go tool cover -html=coverage.txt -o coverage.html
	@echo "coverage report: coverage.html"

# ---------------------------------------------------------------------------
# Build-guard: install a GOFLAGS=-toolexec wrapper that enforces clyde's
# staticcheck on every direct `go build` so out-of-band builds cannot bypass
# the gate. Project-local; not part of the canonical pipeline.
# ---------------------------------------------------------------------------

install-build-guard: ## Enforce repo staticcheck on direct go build via GOFLAGS toolexec
	@./scripts/install-go-build-guard.sh

uninstall-build-guard: ## Remove the repo staticcheck toolexec from GOFLAGS
	@./scripts/install-go-build-guard.sh --uninstall

setup-hooks: ## Configure git hooks
	@git config core.hooksPath .githooks
	@chmod +x .githooks/*
	@echo "git hooks configured"

# ---------------------------------------------------------------------------
# Deploy: install canonical binary and reload daemon.
# Override of canonical install -> service-restart pattern: clyde supports
# `clyde daemon reload` for a hot reload in addition to service restart.
# ---------------------------------------------------------------------------

deploy: install ## Install binary and reload the daemon (or install agent if not loaded)
	@"$(INSTALL_BIN)" daemon reload 2>/dev/null || $(MAKE) service-install

# ---------------------------------------------------------------------------
# Claude Code SessionStart hook (jq-based ~/.claude/settings.json edit).
# Project-local; not part of canonical pipeline.
# ---------------------------------------------------------------------------

install-hook: ## Register the SessionStart hook in ~/.claude/settings.json
	@mkdir -p "$(HOME)/.claude"
	@touch "$(HOME)/.claude/settings.json"
	@if [ ! -s "$(HOME)/.claude/settings.json" ]; then echo '{}' > "$(HOME)/.claude/settings.json"; fi
	@cp "$(HOME)/.claude/settings.json" "$(HOME)/.claude/settings.json.bak.$$(date +%s)"
	@jq --arg cmd "$(INSTALL_BIN) hook sessionstart" \
		'.hooks = (.hooks // {}) | .hooks.SessionStart = (.hooks.SessionStart // []) | \
		 .hooks.SessionStart = (.hooks.SessionStart | map(select(.hooks[0].command != $$cmd))) + \
		 [{matcher: "*", hooks: [{type: "command", command: $$cmd}]}]' \
		"$(HOME)/.claude/settings.json" > "$(HOME)/.claude/settings.json.tmp"
	@mv "$(HOME)/.claude/settings.json.tmp" "$(HOME)/.claude/settings.json"
	@echo "SessionStart hook registered"

uninstall-hook: ## Remove the SessionStart hook from ~/.claude/settings.json
	@if [ -f "$(HOME)/.claude/settings.json" ]; then \
		jq --arg cmd "$(INSTALL_BIN) hook sessionstart" \
			'if .hooks.SessionStart then .hooks.SessionStart |= map(select(.hooks[0].command != $$cmd)) else . end' \
			"$(HOME)/.claude/settings.json" > "$(HOME)/.claude/settings.json.tmp" && \
		mv "$(HOME)/.claude/settings.json.tmp" "$(HOME)/.claude/settings.json"; \
		echo "SessionStart hook removed"; \
	fi
