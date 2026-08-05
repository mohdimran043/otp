# Sender build and test targets.
#
# The test targets start and stop their own Postgres, because the integration tests are the ones
# that matter here and a suite that only runs when somebody remembers to start a database is a
# suite that stops being run.

GO ?= go
PG_CONTAINER ?= otp-sender-test-pg
PG_PORT ?= 55432
TEST_DATABASE_URL ?= postgres://otp:otp@localhost:$(PG_PORT)/otp_sender?sslmode=disable

.PHONY: build run check-config fmt vet test test-unit test-integration pg-up pg-down clean

build:
	$(GO) build -o bin/sender ./cmd/sender

run: build
	./bin/sender -config sender.yaml

check-config: build
	./bin/sender -check-config -config sender.yaml

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

# Everything that needs no database. The integration tests skip rather than fail without one, so
# this is a real pass rather than a partial one.
test-unit:
	$(GO) test ./...

# The full suite, with a database started and torn down around it.
test-integration: pg-up
	OTP_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" $(GO) test ./... -timeout 600s; \
	status=$$?; $(MAKE) pg-down; exit $$status

test: test-integration

pg-up:
	@docker inspect $(PG_CONTAINER) >/dev/null 2>&1 || \
		docker run -d --rm --name $(PG_CONTAINER) \
			-e POSTGRES_USER=otp -e POSTGRES_PASSWORD=otp -e POSTGRES_DB=otp_sender \
			-p $(PG_PORT):5432 postgres:16-alpine >/dev/null
	@for i in $$(seq 1 60); do \
		docker exec $(PG_CONTAINER) pg_isready -U otp -d otp_sender >/dev/null 2>&1 && exit 0; \
		sleep 1; \
	done; \
	echo "postgres did not become ready" >&2; exit 1

pg-down:
	-@docker rm -f $(PG_CONTAINER) >/dev/null 2>&1 || true

clean:
	rm -rf bin
