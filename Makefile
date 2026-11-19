SHELL := /bin/bash

# Pinned so a local mail sink does not change under the developer.
MAILPIT_VERSION ?= v1.34.0

# govulncheck is pinned here rather than in go.mod: it is a build-time tool.
# v1.1.x crashes on Go 1.27; v1.9.0 works.
GOVULNCHECK_VERSION ?= v1.10.0

# Local development: the fake directory (any password), plain http, and mail
# captured by mailpit. None of this is valid for a deployment -- config refuses
# DIRECTORY=fake with secure cookies.
DEV_ENV = DIRECTORY=fake COOKIE_SECURE=false \
	LISTEN_ADDR=:8082 BASE_URL=http://localhost:8082 DB_PATH=data/leave.db

.PHONY: fmt-check vulncheck vulncheck-install run run-mail build test check vet fmt tidy recreate-db mailpit deploy logs-prod migrate-status

## migrate-status: print applied/pending database migrations without starting the server
migrate-status:
	go run . -migrate-status

## run: the server on :8082 with the fake directory (testdata/directory.json)
run:
	$(DEV_ENV) go run .

## run-mail: as run, sending mail to mailpit (start it with make mailpit)
run-mail:
	$(DEV_ENV) DEV_SMTP_HOST=localhost go run .

## recreate-db: DESTROYS the local database, then starts clean
recreate-db:
	rm -f data/leave.db data/leave.db-wal data/leave.db-shm

build:
	go build -o bin/leave .

## test: every test, uncached
test:
	go test -count=1 ./...

## check: the gate before a commit (tests with -race, then the vulnerability scan)
# The skip count is printed because the live LDAP/Graph tests are opt-in and
# skip silently otherwise; two skips is the expected number.
check: fmt-check
	go vet ./...
	@out="$$(mktemp)"; trap 'rm -f "$$out"' EXIT; \
	  go test -count=1 -race -v ./... 2>&1 | tee "$$out" | grep -E '^(FAIL|ok|---\s+FAIL|panic)' ; \
	  rc=$${PIPESTATUS[0]}; \
	  echo "skipped tests: $$(grep -c -E '^\s*--- SKIP' "$$out")"; exit $$rc
	$(MAKE) vulncheck

## fmt-check: fail, listing the files, if any tracked Go source is not gofmt-clean
# Scoped to tracked files so untracked scratch cannot fail the gate.
fmt-check:
	@bad="$$(gofmt -l $$(git ls-files '*.go') 2>&1)"; rc=$$?; \
	if [ $$rc -ne 0 ] || [ -n "$$bad" ]; then echo "not gofmt-clean (or not parseable):"; echo "$$bad"; exit 1; fi

## vulncheck-install: install the pinned govulncheck
vulncheck-install:
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

## vulncheck: fail if a reachable known vulnerability affects the build
vulncheck:
	@command -v govulncheck >/dev/null || { echo "govulncheck missing: make vulncheck-install" >&2; exit 1; }
	govulncheck ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

## mailpit: local SMTP sink and viewer at http://localhost:8025
mailpit:
	docker run --rm -p 1025:1025 -p 8025:8025 axllent/mailpit:$(MAILPIT_VERSION)

# The gem's bin dir is not on PATH everywhere; override with `make deploy KAMAL=/path/to/kamal`.
KAMAL ?= kamal

## deploy: run the check gate and bin/check-env, then `kamal deploy` (build, push to ghcr.io, swap in on sdt-vm-docker02)
deploy: check
	@bin/check-env
	@# Kamal builds from HEAD, not the working tree, so refuse edits the checks just ran against.
	@git diff --quiet HEAD -- || { echo "deploy: tracked files have uncommitted changes; commit first so the tag names what runs" >&2; exit 1; }
	$(KAMAL) deploy

## logs-prod: follow the production app's logs
logs-prod:
	$(KAMAL) app logs -f
