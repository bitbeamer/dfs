BINARY := bin/dfs
.PHONY: build test test-integration test-mount clean

build:
	go build -ldflags "-X github.com/bitbeamer/dfs/internal/cli.Version=$$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o $(BINARY) ./cmd/dfs

test:
	go test ./...

test-integration: build
	DFS_INTEGRATION=1 go test ./internal/integration -v

test-mount:
	@test -n "$(MOUNTPOINT)" || { echo "Usage: make test-mount MOUNTPOINT=/path/to/dfs-mount PEERS='host:/remote/mount ...'" >&2; exit 2; }
	./scripts/test-mounted-volume.sh $(TEST_MOUNT_FLAGS) "$(MOUNTPOINT)" $(PEERS)

clean:
	rm -rf bin
