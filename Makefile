.PHONY: build clean

GO := go
BINDIR := bin

build:
	mkdir -p $(BINDIR)
	$(GO) build -o $(BINDIR)/chunk-inspector ./cmd/chunk-inspector
	$(GO) build -o $(BINDIR)/overlap-analyzer ./cmd/overlap-analyzer
	$(GO) build -o $(BINDIR)/cache-service ./cmd/cache-service
	$(GO) build -o $(BINDIR)/chunk-proxy ./cmd/chunk-proxy
	$(GO) build -o $(BINDIR)/trace-replayer ./cmd/trace-replayer
	$(GO) build -o $(BINDIR)/cache-loadgen ./cmd/cache-loadgen
	$(GO) build -o $(BINDIR)/cache-sim ./cmd/cache-sim

clean:
	rm -rf $(BINDIR)
