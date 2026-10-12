GO ?= go
GOFMT ?= gofmt
VERSION ?= dev
LDFLAGS := -X github.com/KinasTomes/miles/internal/buildinfo.Version=$(VERSION)

.PHONY: build fmt check-fmt vet test check

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/miles ./cmd/miles
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/dsgctl ./cmd/dsgctl

fmt:
	$(GOFMT) -w cmd internal

check-fmt:
	@test -z "$$($(GOFMT) -l cmd internal)" || { echo "Chạy make fmt trước khi commit."; exit 1; }

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

check: check-fmt vet test build
