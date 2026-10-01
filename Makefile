VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo 0.0.0-dev)
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
# CPA's plugin installer expects the platform's native library suffix.
EXT     := $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))
OUT     ?= dist/$(GOOS)/$(GOARCH)/cliproxy-costs.$(EXT)
export GOTOOLCHAIN ?= auto

.PHONY: build test vet check clean

build:
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X main.version=$(VERSION)" -o $(OUT) .
	rm -f $(basename $(OUT)).h

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test build

clean:
	rm -rf dist
