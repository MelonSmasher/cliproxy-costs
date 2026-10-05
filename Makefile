GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo 0.0.0-dev)
GOOS    ?= $(shell $(GO) env GOOS)
GOARCH  ?= $(shell $(GO) env GOARCH)
# CPA's plugin installer expects the platform's native library suffix.
EXT     := $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))
OUT     ?= dist/$(GOOS)/$(GOARCH)/cliproxy-costs.$(EXT)
export GOTOOLCHAIN ?= auto

.PHONY: build test vet check clean integration

build:
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X main.version=$(VERSION)" -o $(OUT) .
	rm -f $(basename $(OUT)).h

test:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: vet test build

# The host must be an unmodified plugin-enabled CLIProxyAPI build (v8.0.15 tested).
# The test starts its own isolated host; never point it at a running deployment.
integration: build
	@test -n "$(CPA_BINARY)" || (echo "Set CPA_BINARY to a CLIProxyAPI binary"; exit 1)
	CPA_REQUIRE_NATIVE=1 CPA_BINARY='$(CPA_BINARY)' CPA_PLUGIN_PATH='$(abspath $(OUT))' $(GO) test -v -count=1 -timeout 3m ./integration

clean:
	rm -rf dist
