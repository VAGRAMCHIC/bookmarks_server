# SPDX-License-Identifier: AGPL-3.0-or-later

BIN      := bookmarks-server
PKG      := ./cmd/bookmarks-server
DIST     := dist
GOFLAGS  := -trimpath
LDFLAGS  := -s -w
PLATFORMS := linux/amd64 linux/arm64 windows/amd64
STATICCHECK ?= $(shell command -v staticcheck 2>/dev/null || echo $(shell go env GOPATH)/bin/staticcheck)

.PHONY: build test lint cross clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BIN) $(PKG)

test:
	go test -race ./...

lint:
	go vet ./...
	$(STATICCHECK) ./...

cross:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		out=$(DIST)/$(BIN)-$$os-$$arch$$ext; \
		echo "$$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done

clean:
	rm -rf $(BIN) $(DIST)
