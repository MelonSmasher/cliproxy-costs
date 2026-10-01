VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo 0.0.0-dev)
GOARCH  ?= $(shell go env GOARCH)
OUT     ?= dist/linux/$(GOARCH)/cliproxy-costs.so
export GOTOOLCHAIN ?= auto

.PHONY: build test vet check clean

build:
	CGO_ENABLED=1 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X main.version=$(VERSION)" -o $(OUT) .
	rm -f $(OUT:.so=.h)

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test build

clean:
	rm -rf dist
