BINARY := bin/dfs
.PHONY: build test test-integration clean

build:
	go build -ldflags "-X github.com/bitbeamer/dfs/internal/cli.Version=$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o $(BINARY) ./cmd/dfs

test:
	go test ./...

test-integration: build
	DFS_INTEGRATION=1 go test ./internal/integration -v

clean:
	rm -rf bin
