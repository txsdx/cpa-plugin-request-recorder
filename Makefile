GO ?= go
GOOS ?= $(shell $(GO) env GOOS)
GOARCH ?= $(shell $(GO) env GOARCH)
VERSION ?= 0.1.0
EXT = so
ifeq ($(GOOS),darwin)
EXT = dylib
endif
ifeq ($(GOOS),windows)
EXT = dll
endif
.PHONY: test build dev package
test:
	$(GO) test -race ./...
	node --check web/app.js
	python3 -m unittest discover -s scripts -p 'test_*.py'
build:
	mkdir -p dist
	CGO_ENABLED=1 $(GO) build -trimpath -buildmode=c-shared -ldflags "-X github.com/txsdx/cpa-plugin-request-recorder/internal/recorder.Version=$(VERSION)" -o dist/request-recorder.$(EXT) ./cmd/plugin
dev:
	$(GO) run ./cmd/recorder
package: build
	python3 scripts/package.py $(VERSION) $(GOOS) $(GOARCH) $(EXT)
