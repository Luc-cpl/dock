GO ?= $(shell if command -v go >/dev/null 2>&1; then command -v go; elif [ -x /usr/local/go/bin/go ]; then printf '%s' /usr/local/go/bin/go; else printf '%s' go; fi)
NPM ?= npm
NPM_CACHE ?= $(or $(TMPDIR),/tmp)/dock-npm-cache
BIN_DIR ?= $(HOME)/.local/bin
GLOBAL_DOCK ?= $(BIN_DIR)/dock
DEV_LAUNCHER := $(CURDIR)/scripts/dock-dev
LOCAL_TEST_BIN := $(CURDIR)/dock

.PHONY: build web test clean link-dev link-global unlink-global

build: web
	$(GO) build -o dock ./cmd/dock

web:
	cd web && $(NPM) ci --cache "$(NPM_CACHE)" && $(NPM) run build
	mkdir -p internal/dock/web/dist
	cp -R web/dist/. internal/dock/web/dist/

test:
	$(GO) test ./...

# Link a small launcher that rebuilds the Go app in development mode each time
# the global `dock` command runs. It preserves the caller's working directory.
link-dev:
	@mkdir -p "$(BIN_DIR)"
	@if [ -e "$(GLOBAL_DOCK)" ] && [ ! -L "$(GLOBAL_DOCK)" ]; then \
		echo "Refusing to replace a regular file at $(GLOBAL_DOCK)" >&2; exit 1; \
	fi
	@if [ -L "$(GLOBAL_DOCK)" ]; then \
		current=$$(readlink "$(GLOBAL_DOCK)"); \
		case "$$current" in "$(DEV_LAUNCHER)"|"$(LOCAL_TEST_BIN)") ;; \
			*) echo "Refusing to replace an unrelated dock link at $(GLOBAL_DOCK)" >&2; exit 1 ;; \
		esac; \
	fi
	ln -sfn "$(DEV_LAUNCHER)" "$(GLOBAL_DOCK)"
	@echo "Development command linked: $(GLOBAL_DOCK) -> $(DEV_LAUNCHER)"

# Build a standalone workspace binary and link it globally for local release
# testing. This does not publish or install a system package.
link-global: build
	@mkdir -p "$(BIN_DIR)"
	@if [ -e "$(GLOBAL_DOCK)" ] && [ ! -L "$(GLOBAL_DOCK)" ]; then \
		echo "Refusing to replace a regular file at $(GLOBAL_DOCK)" >&2; exit 1; \
	fi
	@if [ -L "$(GLOBAL_DOCK)" ]; then \
		current=$$(readlink "$(GLOBAL_DOCK)"); \
		case "$$current" in "$(DEV_LAUNCHER)"|"$(LOCAL_TEST_BIN)") ;; \
			*) echo "Refusing to replace an unrelated dock link at $(GLOBAL_DOCK)" >&2; exit 1 ;; \
		esac; \
	fi
	ln -sfn "$(LOCAL_TEST_BIN)" "$(GLOBAL_DOCK)"
	@echo "Local test command linked: $(GLOBAL_DOCK) -> $(LOCAL_TEST_BIN)"

# Remove only the global link created by this workspace.
unlink-global:
	@if [ -L "$(GLOBAL_DOCK)" ]; then \
		current=$$(readlink "$(GLOBAL_DOCK)"); \
		case "$$current" in "$(DEV_LAUNCHER)"|"$(LOCAL_TEST_BIN)") rm "$(GLOBAL_DOCK)"; echo "Removed $(GLOBAL_DOCK)" ;; \
			*) echo "Refusing to remove an unrelated dock link at $(GLOBAL_DOCK)" >&2; exit 1 ;; \
		esac; \
	elif [ -e "$(GLOBAL_DOCK)" ]; then \
		echo "Refusing to remove a non-link file at $(GLOBAL_DOCK)" >&2; exit 1; \
	else \
		echo "No workspace dock link at $(GLOBAL_DOCK)"; \
	fi

clean:
	@if [ -L "$(GLOBAL_DOCK)" ] && [ "$$(readlink "$(GLOBAL_DOCK)")" = "$(LOCAL_TEST_BIN)" ]; then \
		echo "The global command points to ./dock; run 'make unlink-global' before cleaning." >&2; exit 1; \
	fi
	rm -f dock
	rm -rf .build web/dist
