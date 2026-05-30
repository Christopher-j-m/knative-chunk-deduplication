.PHONY: build clean

GO := go
BINDIR := bin

build:
	mkdir -p $(BINDIR)
	$(GO) build -o $(BINDIR)/chunk-inspector ./cmd/chunk-inspector
	$(GO) build -o $(BINDIR)/overlap-analyzer ./cmd/overlap-analyzer
	$(GO) build -o $(BINDIR)/cache-service ./cmd/cache-service

clean:
	rm -rf $(BINDIR)
