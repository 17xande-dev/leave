SHELL := /bin/bash

IMAGE ?= ghcr.io/17xande-dev/leave
TAG ?= $(shell git rev-parse --short HEAD)
# The server is x86_64 and go-sqlite3 needs cgo, so the image is
# architecture-specific; naming the platform keeps an arm workstation honest.
PLATFORM ?= linux/amd64

# Local development: the fake directory (any password), plain http, and mail
# captured by mailpit. None of this is valid for a deployment -- config refuses
# DIRECTORY=fake with secure cookies.
DEV_ENV = DIRECTORY=fake COOKIE_SECURE=false FINAL_APPROVER=pastor \
	LISTEN_ADDR=:8082 BASE_URL=http://localhost:8082 DB_PATH=data/leave.db

.PHONY: run run-mail build test check vet fmt tidy image publish check-clean recreate-db mailpit

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

## check: the gate before a commit
check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go test -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

## mailpit: local SMTP sink and viewer at http://localhost:8025
mailpit:
	docker run --rm -p 1025:1025 -p 8025:8025 axllent/mailpit

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
