GO ?= go
GOLANGCI_LINT_VERSION ?= v2.4.0
GOLANGCI_LINT := $(shell $(GO) env GOPATH)/bin/golangci-lint
GORELEASER_VERSION ?= v2.11.2
GORELEASER := $(GO) run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

.PHONY: all format vet lint lint-fix test fast full release-check snapshot install-smoke clean

all: fast

format:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

$(GOLANGCI_LINT):
	GOBIN=$(dir $(GOLANGCI_LINT)) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix

test:
	$(GO) test ./...

fast: format vet lint test

# The release build is the cross-compile: goreleaser owns the flags for every
# darwin/linux/windows x amd64/arm64 target, so nothing here can drift from
# what a tag actually ships.
full: release-check snapshot install-smoke

release-check:
	$(GORELEASER) check

snapshot:
	$(GORELEASER) release --snapshot --clean --skip=publish

install-smoke: snapshot
	./scripts/install-smoke.sh

clean:
	rm -rf dist
