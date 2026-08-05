# Everything runs in containers.
#
# The point of the default target being dockerized is that a clone of this repository plus Docker is
# enough: no Go toolchain, no Postgres, no MinIO on the host. `make test-local` exists for working on
# the code with a toolchain already installed, and skips whatever it cannot reach.

COMPOSE ?= docker compose -f docker-compose.test.yml

.PHONY: test test-local bench fmt vet clean-test ui ui-install ui-build

# The whole suite, in containers, exiting with the suite's status.
test:
	$(COMPOSE) up --build --abort-on-container-exit --exit-code-from tests
	$(COMPOSE) down -v

# The same suite on the host, for a working loop that does not pay for container startup. Tests that
# need a database skip rather than fail when one is not reachable.
test-local:
	cd shared && go test ./... -count=1 -timeout 900s
	cd sender && go test ./... -count=1 -timeout 900s
	cd receiver && go test ./... -count=1 -timeout 900s
	cd e2e && go test ./... -count=1 -timeout 1800s

# The throughput measurements the README quotes, regenerated.
bench:
	$(COMPOSE) run --rm tests ./deploy/run-benchmarks.sh
	$(COMPOSE) down -v

fmt:
	cd shared && gofmt -w .
	cd sender && gofmt -w .
	cd receiver && gofmt -w .
	cd e2e && gofmt -w .

vet:
	cd shared && go vet ./...
	cd sender && go vet ./...
	cd receiver && go vet ./...
	cd e2e && go vet ./...

clean-test:
	$(COMPOSE) down -v
