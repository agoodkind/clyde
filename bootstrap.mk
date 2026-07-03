# bootstrap.mk: tiny shim that fetches go-makefile assets and includes them.
# Consumer Makefiles set their identity vars (BINARY, CMD, VPKG, MODULES, etc.)
# then `include bootstrap.mk`. Everything else (go.mk, golangci.yml, modules)
# is fetched at parse time and -included transitively.
#
# This file is canonical in agoodkind/go-makefile. Consumers commit a copy.
# Update path: edit go-makefile/bootstrap.mk, then refresh all consumer copies
# (one-off sync; not a long-term mechanism).

GO_MK_DEV_DIR  ?=
GO_MK_MODULES  ?=
GO_MK          := .make/go.mk
GO_MK_BASE_URL ?= https://raw.githubusercontent.com/agoodkind/go-makefile/main
GO_MK_API_REPO ?= agoodkind/go-makefile
GO_MK_API_REF  ?= main

# _go_mk_fetch exists here because bootstrap.mk must fetch go.mk before any
# go.mk helpers are available. After go.mk is included, go.mk:go-mk-fetch-one
# owns sibling module/config fetches.
# Fetch chain at parse time: dev override > gh api (authenticated) > raw URL.
# TODO(fetch-order): keep this order aligned with go.mk:go-mk-fetch-one.
# TODO(moratorium): on-disk cache fallback removed; restore once primary path
# is demonstrably reliable. Until then fail loud rather than serve stale.
define _go_mk_fetch
	if [ -n "$(GO_MK_DEV_DIR)" ] && [ -f "$(GO_MK_DEV_DIR)/$(1)" ]; then \
		cp "$(GO_MK_DEV_DIR)/$(1)" "$(2)"; \
	elif command -v gh >/dev/null 2>&1 && gh api "repos/$(GO_MK_API_REPO)/contents/$(1)?ref=$(GO_MK_API_REF)" -H "Accept: application/vnd.github.raw" > "$(2)" 2>/dev/null && [ -s "$(2)" ]; then \
		: ; \
	elif curl -fsSL --connect-timeout 5 --max-time 10 "$(GO_MK_BASE_URL)/$(1)" -o "$(2)" 2>/dev/null && [ -s "$(2)" ]; then \
		: ; \
	else \
		printf '%s\n' "error: $(1) fetch failed; no cache fallback (moratorium). Set GO_MK_DEV_DIR, install/authenticate gh, or check curl access to $(GO_MK_BASE_URL)" >&2; \
		exit 1; \
	fi
endef

GO_MK_BOOTSTRAP_FETCHED := 1

define _go_mk_require_fetched
$(if $(wildcard $(1)),,$(error go-makefile expected $(1); rerun without GO_MK_SKIP_FETCH))
endef

ifeq ($(strip $(GO_MK_SKIP_FETCH)),1)
GO_MK_FETCH_CHECK := $(call _go_mk_require_fetched,$(GO_MK))
GO_MK_FETCH_CHECK += $(call _go_mk_require_fetched,.make/golangci.yml)
GO_MK_FETCH_CHECK += $(foreach m,$(GO_MK_MODULES),$(call _go_mk_require_fetched,.make/$(m)))
else

$(shell mkdir -p .make && { $(call _go_mk_fetch,go.mk,$(GO_MK)); } 1>&2)
$(shell { $(call _go_mk_fetch,golangci.yml,.make/golangci.yml); } 1>&2)
$(foreach m,$(GO_MK_MODULES),$(shell { $(call _go_mk_fetch,$(m),.make/$(m)); } 1>&2))

endif

# go.mk handles -including the modules at its tail (after all its variables
# are defined), so the modules see build-check etc. Don't duplicate
# the include here or every module target gets overriding-commands warnings.
-include $(GO_MK)
