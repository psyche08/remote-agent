.PHONY: go-build go-test go-vet go-run go-clean

GO ?= go
GOCACHE ?= /private/tmp/agenthalo-gocache
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
MODULE_VERSION ?= $(shell tr -d '[:space:]' < VERSION)
BUILT_AT ?= $(shell TZ=Asia/Shanghai date +%Y-%m-%dT%H:%M:%S+08:00)
BUILDINFO_PKG := github.com/psyche08/remote-agent/internal/buildinfo
LDFLAGS := -X $(BUILDINFO_PKG).Version=AgentHalo.$(MODULE_VERSION) -X $(BUILDINFO_PKG).Commit=$(COMMIT) -X $(BUILDINFO_PKG).BuiltAt=$(BUILT_AT)

go-build:
	GOCACHE=$(GOCACHE) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/agenthalo ./cmd/agenthalo

go-test:
	GOCACHE=$(GOCACHE) $(GO) test ./...

go-vet:
	GOCACHE=$(GOCACHE) $(GO) vet ./...

go-run:
	GOCACHE=$(GOCACHE) $(GO) run ./cmd/agenthalo --listen 127.0.0.1:18765

go-clean:
	rm -rf bin
