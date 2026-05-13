# Lint is centralized in go-makefile. Do NOT define project-local lint,
# audit, fmt, vet, or staticcheck targets here. They duplicate the central
# pipeline and let agents bypass strict rules. Run `make help` for the
# canonical entry points (build/check/lint/fmt) and per-linter sub-targets
# (lint-golangci, lint-format, lint-gocyclo, lint-deadcode,
# staticcheck-extra). Refresh baselines via the matching *-baseline target.
#
# clyde Makefile.
# Build/lint/release/service pipeline lives in go-makefile and is fetched at
# runtime. Project-local: clyde-staticcheck custom analyzer, deadcode allowlist,
# ginkgo as an alternate test runner, build-guard hook, and the SessionStart
# hook installer.

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

# Exclude protobuf-generated code under /api/ from staticcheck-extra.
STATICCHECK_EXTRA_EXCLUDE_PATHS = \.pb\.go:,/api/

# Project allowlist for the central lint-deadcode gate.
DEADCODE_EXCLUDE_PATHS = cmd/root.go:.*NewRootCmd,internal/mitm/(baseline_paths|codegen|codegen_v2|drift_runner)\.go:,internal/testutil/claude.go:.*CreateFakeClaude,internal/testutil/claude.go:.*ReadClaudeArgs

# Pipeline modules
GO_MK_MODULES := go-build.mk go-release.mk go-service.mk

include bootstrap.mk

.DEFAULT_GOAL := check

# ---------------------------------------------------------------------------
# Project-local
# ---------------------------------------------------------------------------

BUNDLE_ID         ?= io.goodkind.clyde
CODESIGN_IDENTITY := $(or $(CERT_ID),$(shell if [ "$$(uname)" = "Darwin" ]; then security find-identity -v -p codesigning 2>/dev/null | awk '/Developer ID Application/ { print $$2; exit }'; fi))

.PHONY: test-ginkgo test-watch coverage \
        install-build-guard uninstall-build-guard setup-hooks \
        install-hook uninstall-hook \
        deploy deadcode

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

deadcode: lint-deadcode ## Alias for the central deadcode gate

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
# Deploy: install canonical binary, ensure supervisor ownership, reload daemon.
# Clyde keeps `clyde daemon reload` as the binary-handoff path, but launchd or
# systemd must own normal deployed daemon startup before the reload RPC runs.
# ---------------------------------------------------------------------------

deploy: install ## Install binary, ensure supervisor ownership, reload daemon, and print service status
	@$(MAKE) service-install
	@"$(INSTALL_BIN)" daemon reload
	@$(MAKE) service-status

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
