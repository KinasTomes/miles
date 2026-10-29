GO ?= go
GOFMT ?= gofmt
VERSION ?= dev
FUZZTIME ?= 30s
LDFLAGS := -X github.com/KinasTomes/miles/internal/buildinfo.Version=$(VERSION)

.PHONY: build fmt check-fmt vet test check run-dev test-local fuzz

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/miles ./cmd/miles
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/dsgctl ./cmd/dsgctl
	$(GO) build -trimpath -o bin/warmbench ./cmd/warmbench

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

# Fuzz parser S3 (SigV4, aws-chunked/trailer, XML, route) trên input ngẫu
# nhiên trong FUZZTIME mỗi target; seed corpus đã chạy trong `make test`.
fuzz:
	$(GO) test ./internal/api/s3 -run '^$$' -fuzz '^FuzzResolve$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/api/s3 -run '^$$' -fuzz '^FuzzSigV4$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/api/s3 -run '^$$' -fuzz '^FuzzAWSChunked$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/api/s3 -run '^$$' -fuzz '^FuzzDecodeXML$$' -fuzztime=$(FUZZTIME)

run-dev: build
	./bin/miles -config configs/dev.json

test-local:
	python3 -B scripts/local_test.py
