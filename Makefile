GOOS   ?= linux
GOARCH ?= amd64

GO_LDFLAGS := -ldflags "-s -w"
PKG        := ./cmd/kimi-monitor/
DST        := ./bin
BINARY     := kimi_monitor

.PHONY: build clean

build:
	@mkdir -p $(DST)
	@GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GO_LDFLAGS) -o $(DST)/$(BINARY) $(PKG)

clean:
	@go clean
	@rm -rf $(DST)
