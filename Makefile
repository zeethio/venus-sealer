SHELL=/usr/bin/env bash

CLEAN:=
BINS:=

ldflags=-X=github.com/filecoin-project/venus-sealer/constants.CurrentCommit=+git.$(subst -,.,$(shell git describe --always --match=NeVeRmAtCh --dirty 2>/dev/null || git rev-parse --short HEAD 2>/dev/null))
ifneq ($(strip $(LDFLAGS)),)
	ldflags+=-extldflags=$(LDFLAGS)
endif

GOFLAGS+=-ldflags="$(ldflags)"

## FFI

FFI_PATH:=extern/filecoin-ffi/

CLEAN+=build/.filecoin-install

build:
	go build $(GOFLAGS) -o venus-sealer ./app/venus-sealer
	go build $(GOFLAGS) -o venus-worker ./app/venus-worker
	BINS+=venus-sealer
	BINS+=venus-worker

deps:
	git submodule update --init
	./extern/filecoin-ffi/install-filcrypto

lint:
	go run github.com/golangci/golangci-lint/cmd/golangci-lint run

clean:
	rm -rf $(CLEAN) $(BINS)
	-$(MAKE) -C $(FFI_PATH) clean
.PHONY: clean
