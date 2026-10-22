GO ?= go
GOFMT ?= gofmt
VERSION ?= dev
LDFLAGS := -X github.com/KinasTomes/miles/internal/buildinfo.Version=$(VERSION)

.PHONY: build fmt check-fmt vet test check run-dev test-local

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/miles ./cmd/miles
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/dsgctl ./cmd/dsgctl

fmt:
	$(GOFMT) -w cmd internal configs/pricing

check-fmt:
	@test -z "$$($(GOFMT) -l cmd internal configs/pricing)" || { echo "Chạy make fmt trước khi commit."; exit 1; }

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...
	python3 -B -m unittest discover -s scripts/tests -v
	python3 -B scripts/check_doc_names.py

check: check-fmt vet test build

run-dev: build
	./bin/miles -config configs/dev.json

test-local:
	python3 -B scripts/local_test.py
