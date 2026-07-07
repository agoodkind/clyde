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
# generated protobuf code and ginkgo as an alternate test runner.

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

# Pipeline modules
GO_MK_MODULES := go-build.mk go-release.mk go-service.mk

include bootstrap.mk

.DEFAULT_GOAL := check

# ---------------------------------------------------------------------------
# Project-local
# ---------------------------------------------------------------------------

BUNDLE_ID         ?= io.goodkind.clyde
CODESIGN_IDENTITY := $(or $(CERT_ID),$(shell if [ "$$(uname)" = "Darwin" ]; then security find-identity -v -p codesigning 2>/dev/null | awk '/Developer ID Application/ { print $$2; exit }'; fi))

.PHONY: test-ginkgo test-watch coverage live setup-hooks install-hooks \
        deploy daemon-reload deadcode proto

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
# CLYDE_TEST_CURSOR_PORT, CLYDE_TEST_TOPOLOGY_PORT, CLYDE_TEST_MOVED_MITM_PORT.
live: ## Run the live daemon validation suite (opt-in, build tag live)
	@go test -tags live -count=1 ./test/live/

deadcode: lint-deadcode ## Alias for the central deadcode gate

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
