# RACE is empty locally (no cgo on the dev machine); CI passes RACE=-race
RACE ?=

# golangci-lint v2.13.0, the version pinned in .github/workflows/ci.yml
# (go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.0)
GOLANGCI_LINT ?= golangci-lint

# replace filename-unsafe branch characters, including /, with -
BRANCH := $(shell git rev-parse --abbrev-ref HEAD | LC_ALL=C sed 's/[^A-Za-z0-9._-]/-/g')
EXE := $(shell go env GOEXE)
BINARY := .bin/agrouter.$(BRANCH)$(EXE)

all: test build

build:
	go build -o '$(BINARY)' ./cmd/agrouter

test:
	go clean -testcache
	go test $(RACE) -coverprofile=coverage.out ./...
	grep -v "_mock.go" coverage.out | grep -v mocks > coverage_no_mocks.out
	go tool cover -func=coverage_no_mocks.out
	rm coverage.out coverage_no_mocks.out

lint:
	$(GOLANGCI_LINT) run --max-issues-per-linter=0 --max-same-issues=0

fmt:
	gofmt -s -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")
	goimports -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")

generate:
	go generate ./...

# routes testdata/routing against the real Jev API (needs TYPESAFE_API_KEY), never in CI
eval-routing:
	@test -n "$$TYPESAFE_API_KEY" || { echo "eval-routing: TYPESAFE_API_KEY is not set"; exit 1; }
	go test -tags=eval -count=1 -v -run TestEvalRouting -timeout 60m ./pkg/router/...

.PHONY: all build test lint fmt generate eval-routing
