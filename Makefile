SHELL := /bin/bash

IMAGE ?= ghcr.io/17xande-dev/leave
TAG ?= $(shell git rev-parse --short HEAD)
# The server is x86_64 and go-sqlite3 needs cgo, so the image is
# architecture-specific; naming the platform keeps an arm workstation honest.
PLATFORM ?= linux/amd64

# Pinned so a local mail sink does not change under the developer.
MAILPIT_VERSION ?= v1.32.0

# govulncheck is pinned here rather than in go.mod: it is a build-time tool.
# v1.1.x crashes on Go 1.27; v1.9.0 works.
GOVULNCHECK_VERSION ?= v1.9.0

# Local development: the fake directory (any password), plain http, and mail
# captured by mailpit. None of this is valid for a deployment -- config refuses
# DIRECTORY=fake with secure cookies.
DEV_ENV = DIRECTORY=fake COOKIE_SECURE=false \
	LISTEN_ADDR=:8082 BASE_URL=http://localhost:8082 DB_PATH=data/leave.db

# Deployment target. The address, user and key live in ~/.ssh/config under
# this alias, never in the repo:
#   Host docker-server
#     HostName <the server's address>
#     User <you>
DEPLOY_HOST ?= docker-server
DEPLOY_DIR ?= ~/leave
# HTTPS, not SSH: the repo is public, so the server needs no key to read it,
# and an ssh:// clone fails with "Host key verification failed" on a server
# that has never connected to github.com (nobody is there to accept the key).
REPO ?= https://github.com/17xande-dev/leave.git

.PHONY: fmt-check vulncheck vulncheck-install run run-mail build test check vet fmt tidy image publish check-clean recreate-db mailpit deploy check-pushed

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

image:
	docker build --platform $(PLATFORM) -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

check-clean:
	@if [ -n "$(ALLOW_DIRTY)" ]; then exit 0; fi; \
	if [ -n "$$(git status --porcelain)" ]; then \
	  echo "refusing to publish: uncommitted changes (commit them, or ALLOW_DIRTY=1)" >&2; exit 1; fi

## publish: build and push the image; TAG is the commit, so a dirty tree is refused
publish: check-clean image
	docker push $(IMAGE):$(TAG)
	docker push $(IMAGE):latest
	@echo "on the server: set TAG=$(TAG) in .env, then docker compose pull && docker compose up -d"

# check-pushed: refuse when the server would get something other than what is
# here. deploy pulls from GitHub, so an uncommitted edit or an unpushed commit
# would quietly not be deployed while looking as though it had been.
check-pushed:
	@if [ -n "$$(git status --porcelain)" ]; then \
	  echo "refusing to deploy: uncommitted changes" >&2; git status --short >&2; exit 1; fi
	@git fetch -q origin
	@if [ "$$(git rev-parse HEAD)" != "$$(git rev-parse @{u})" ]; then \
	  echo "refusing to deploy: HEAD is not what origin has; push (or pull) first" >&2; exit 1; fi

## deploy: clone or update the repo on the server, then rebuild and restart
# .env lives only on the server; the first deploy stops and asks for it rather
# than starting a container with no configuration.
deploy: check-pushed
	ssh $(DEPLOY_HOST) 'set -e; \
	  if [ -d $(DEPLOY_DIR)/.git ]; then git -C $(DEPLOY_DIR) pull --ff-only; \
	  else git clone $(REPO) $(DEPLOY_DIR); fi; \
	  cd $(DEPLOY_DIR); \
	  if [ ! -f .env ]; then echo "no .env in $(DEPLOY_DIR) on the server: copy .env.example, fill it in, deploy again" >&2; exit 1; fi; \
	  echo "deploying $$(git rev-parse --short HEAD)"; \
	  docker compose up -d --build --remove-orphans; \
	  docker compose ps'
