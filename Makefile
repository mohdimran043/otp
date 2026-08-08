# Everything runs in containers, when the containerised stack is part of the checkout.
#
# The point of the default target being dockerized is that a clone of this repository plus Docker is
# enough: no Go toolchain, no Postgres, no MinIO on the host. `make test-local` exists for working on
# the code with a toolchain already installed, and skips whatever it cannot reach.
#
# The compose file and the end-to-end suite are kept outside the repository, so every target that needs
# one checks for it first. A checkout without them runs what it has and says what it could not run —
# the failure worth avoiding is not a broken target, it is a green result that covered less than it
# looks like it did.

COMPOSE_FILE ?= docker-compose.test.yml
COMPOSE ?= docker compose -f $(COMPOSE_FILE)

.PHONY: test test-local bench fmt vet clean-test

# The whole suite, in containers, exiting with the suite's status.
#
# `down -v` runs whether or not the suite passed, and the suite's status is what the target exits with.
# As two plain recipe lines a failing suite aborted the target before the teardown, leaving the
# containers and their volumes behind on exactly the runs where somebody was about to look again.
test:
	@if [ -f $(COMPOSE_FILE) ]; then \
	    $(COMPOSE) up --build --abort-on-container-exit --exit-code-from tests; \
	    status=$$?; \
	    $(COMPOSE) down -v; \
	    exit $$status; \
	else \
	    echo "  $(COMPOSE_FILE) is not in this checkout — running the host suite instead."; \
	    echo "  That needs a Go toolchain, and anything requiring Postgres or MinIO will skip"; \
	    echo "  rather than run. Read the skips: a pass here is a narrower claim."; \
	    echo ""; \
	    $(MAKE) --no-print-directory test-local; \
	fi

# The same suite on the host, for a working loop that does not pay for container startup. Tests that
# need a database skip rather than fail when one is not reachable.
test-local:
	cd shared && go test ./... -count=1 -timeout 900s
	cd sender && go test ./... -count=1 -timeout 900s
	cd receiver && go test ./... -count=1 -timeout 900s
	@if [ -d e2e ]; then \
	    cd e2e && go test ./... -count=1 -timeout 1800s; \
	else \
	    echo "  end-to-end loopback: skipped — no e2e directory in this checkout"; \
	fi

# The throughput measurements the README quotes, regenerated.
bench:
	@if [ -f $(COMPOSE_FILE) ]; then \
	    $(COMPOSE) run --rm tests ./deploy/run-benchmarks.sh; \
	    status=$$?; \
	    $(COMPOSE) down -v; \
	    exit $$status; \
	else \
	    echo "  bench needs $(COMPOSE_FILE), which is not in this checkout."; \
	    exit 1; \
	fi

fmt:
	cd shared && gofmt -w .
	cd sender && gofmt -w .
	cd receiver && gofmt -w .
	@if [ -d e2e ]; then cd e2e && gofmt -w .; fi

vet:
	cd shared && go vet ./...
	cd sender && go vet ./...
	cd receiver && go vet ./...
	@if [ -d e2e ]; then cd e2e && go vet ./...; fi

clean-test:
	@if [ -f $(COMPOSE_FILE) ]; then \
	    $(COMPOSE) down -v; \
	else \
	    echo "  nothing to clean: $(COMPOSE_FILE) is not in this checkout"; \
	fi
