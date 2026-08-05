# bootstrap.mk: tiny shim that obtains the go-makefile bootstrap helper, runs
# it, and includes the engine. Consumer Makefiles set their identity vars
# (BINARY, CMD, VPKG, MODULES, etc.) then `include bootstrap.mk`.
#
# This file is canonical in agoodkind/go-makefile. Consumers commit a copy.
# It deliberately holds no fetch policy beyond obtaining the helper: the helper
# is fetched, so a change to validation, reuse, or failure behavior reaches
# every consumer on its next run with no consumer-side change.

GO_MK_DEV_DIR  ?=
GO_MK_MODULES  ?=
GO_MK          := .make/go.mk
GO_MK_BASE_URL ?= https://raw.githubusercontent.com/agoodkind/go-makefile/main
GO_MK_API_REPO ?= agoodkind/go-makefile
GO_MK_API_REF  ?= main

GO_MK_BOOTSTRAP := .make/scripts/go-mk-bootstrap.sh
# GO_MK_BOOTSTRAP_BASE_URL is an internal override in the same category as
# GO_MK_CODELOAD_BASE in go-mk-bootstrap.sh: tests point it at a local server,
# consumers never set it. The helper URL itself follows GO_MK_API_REF so a
# ref-pinned consumer gets that ref's helper. GO_MK_BASE_URL ends in /main and
# would pin the helper to main, so it is not used here.
GO_MK_BOOTSTRAP_BASE_URL ?= https://raw.githubusercontent.com
GO_MK_BOOTSTRAP_URL := $(GO_MK_BOOTSTRAP_BASE_URL)/$(GO_MK_API_REPO)/$(GO_MK_API_REF)/scripts/go-mk-bootstrap.sh

# Obtaining the helper is the only fetch rule left in consumer-committed code.
# It never removes an existing helper, so a warm checkout stays usable when the
# network is gone, and only a cold offline start fails here.
#
# This is a consumer-committed fetch, so it cannot be hardened later the way
# a fetched file can: any future change here needs another consumer PR, which
# is exactly the round this task exists to end. The curl flags below give it
# the same treatment provision() in go-mk-bootstrap.sh got. --speed-limit/
# --speed-time abort a stalled connection by lack of progress rather than
# elapsed time (a stall dies in ~3s instead of riding --max-time out), and
# --retry-max-time caps the retry cascade at two attempts instead of leaving
# it unbounded (curl treats a speed-limit or max-time abort as a retriable
# transient error, so uncapped retries would cost roughly 4x --max-time).
# --connect-timeout is 5, not tighter, for the same reason provision()'s is:
# connect time (DNS, TCP, TLS setup) has nothing to do with stall detection,
# and this is the very first network call a cold consumer makes, so it must
# not fail a slow-but-working link before retrying has a chance to help.
define _go_mk_get_bootstrap
	if [ -n "$(GO_MK_DEV_DIR)" ] && [ -f "$(GO_MK_DEV_DIR)/scripts/go-mk-bootstrap.sh" ]; then \
		mkdir -p .make/scripts; \
		devtmp=$$(mktemp "$(GO_MK_BOOTSTRAP).tmp.XXXXXX") || exit 1; \
		if cp "$(GO_MK_DEV_DIR)/scripts/go-mk-bootstrap.sh" "$$devtmp" && mv "$$devtmp" "$(GO_MK_BOOTSTRAP)"; then \
			: ; \
		else \
			rm -f "$$devtmp"; \
			printf '%s\n' "error: could not install $(GO_MK_BOOTSTRAP) from GO_MK_DEV_DIR=$(GO_MK_DEV_DIR)" >&2; \
			exit 1; \
		fi; \
	elif [ -s "$(GO_MK_BOOTSTRAP)" ]; then \
		: ; \
	else \
		mkdir -p .make/scripts; \
		tmp=$$(mktemp "$(GO_MK_BOOTSTRAP).tmp.XXXXXX") || exit 1; \
		if curl -fsSL --connect-timeout 5 --max-time 15 \
			--speed-limit 1024 --speed-time 3 \
			--retry 3 --retry-delay 2 --retry-max-time 4 \
			"$(GO_MK_BOOTSTRAP_URL)" -o "$$tmp" 2>/dev/null && [ -s "$$tmp" ]; then \
			mv "$$tmp" "$(GO_MK_BOOTSTRAP)"; \
		else \
			rm -f "$$tmp"; \
			printf '%s\n' "error: could not obtain $(GO_MK_BOOTSTRAP). Set GO_MK_DEV_DIR, or check network access to $(GO_MK_BOOTSTRAP_BASE_URL)" >&2; \
			exit 1; \
		fi; \
	fi; \
	chmod +x "$(GO_MK_BOOTSTRAP)"
endef

GO_MK_BOOTSTRAP_FETCHED := 1

ifeq ($(strip $(GO_MK_SKIP_FETCH)),1)
# Test for a non-empty regular file, not merely an existing path. $(wildcard)
# reports any name that exists, including a zero-byte file, and bash exits 0 on
# an empty script, so GO_MK_PROVISION would come back `ok` and the parse would
# continue with no engine provisioned at all. That is reachable: the fetch path
# below writes a temp file and renames it, but an interrupted earlier run, a
# full disk, or a hand-created placeholder all leave an empty helper that this
# guard is the only thing standing in front of. -s matches the same test the
# fetch path uses on the cached helper, so both paths agree on what counts as
# present.
GO_MK_BOOTSTRAP_PRESENT := $(shell test -s "$(GO_MK_BOOTSTRAP)" && printf yes)
$(if $(GO_MK_BOOTSTRAP_PRESENT),,$(error go-makefile expected a non-empty $(GO_MK_BOOTSTRAP); rerun without GO_MK_SKIP_FETCH))
else
$(shell { $(call _go_mk_get_bootstrap); } 1>&2)
endif

# The helper provisions every asset and owns the validation, reuse, and failure
# rules. A non-zero exit means it could not produce a usable .make, so stop
# rather than parse an engine that is not there.
#
# Every GO_MK_* variable THE HELPER READS is forwarded explicitly. That is the
# six below, which is the complete set the helper references.
#
# GO_MK_BASE_URL and GO_MK_BOOTSTRAP_BASE_URL are deliberately not among them:
# the helper never reads either. GO_MK_BASE_URL belongs to go.mk's own fetch
# path, and GO_MK_BOOTSTRAP_BASE_URL is consumed by this file when it acquires
# the helper, before the helper runs. Forwarding a variable the helper ignores
# would suggest it has an effect there.
#
# Make only auto-exports variables that came from the process
# environment, so a consumer who sets one on the make command line, or with a
# plain assignment in their own Makefile before this include, sets the Make
# variable without exporting it. This file then acts on the value while the
# helper, which owns every asset install, never sees it, and the two halves
# disagree about what the user asked for.
#
# That split has already produced three distinct bugs here, so forward the
# whole set rather than adding names one at a time as each is found:
#
#   GO_MK_DEV_DIR       this file takes its dev branch while the helper
#                       downloads upstream over the developer's own checkout,
#                       so they build and lint against main believing they
#                       are testing local edits
#   GO_MK_SKIP_FETCH    this file honors it while the helper fetches anyway,
#                       so an air-gapped or pre-vendored build fails at parse
#                       time, the exact case the flag exists to serve
#   GO_MK_CODELOAD_BASE the redirect is silently ineffective and the helper
#                       reaches real codeload while appearing redirected, so
#                       a test written that way passes against production
#   GO_MK_API_REPO      the helper falls back to its own defaults and fetches
#   GO_MK_API_REF       the wrong repository or ref's assets
#
# Adding a GO_MK_* variable that the helper reads means adding it here too.
GO_MK_PROVISION := $(shell GO_MK_API_REPO="$(GO_MK_API_REPO)" GO_MK_API_REF="$(GO_MK_API_REF)" GO_MK_MODULES="$(GO_MK_MODULES)" GO_MK_CODELOAD_BASE="$(GO_MK_CODELOAD_BASE)" GO_MK_DEV_DIR="$(GO_MK_DEV_DIR)" GO_MK_SKIP_FETCH="$(GO_MK_SKIP_FETCH)" bash "$(GO_MK_BOOTSTRAP)" >&2 && printf ok)
$(if $(filter ok,$(GO_MK_PROVISION)),,$(error go-makefile failed to provision its assets))

# go.mk handles -including the modules at its tail (after all its variables
# are defined), so the modules see build-check etc. Don't duplicate
# the include here or every module target gets overriding-commands warnings.
-include $(GO_MK)
