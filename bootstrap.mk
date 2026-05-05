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

# Fetch chain at parse time: dev override > gh api (authenticated) > raw URL.
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
		printf '%s\n' "error: $(1) fetch failed; no cache fallback (moratorium). Run: gh auth login" >&2; \
		exit 1; \
	fi
endef

$(shell mkdir -p .make && { $(call _go_mk_fetch,go.mk,$(GO_MK)); } 1>&2)
$(shell { $(call _go_mk_fetch,golangci.yml,.make/golangci.yml); } 1>&2)
$(foreach m,$(GO_MK_MODULES),$(shell { $(call _go_mk_fetch,$(m),.make/$(m)); } 1>&2))

# go.mk handles -including the modules at its tail (after all its variables
# are defined), so the modules see default-build-deps etc. Don't duplicate
# the include here or every module target gets overriding-commands warnings.
-include $(GO_MK)
