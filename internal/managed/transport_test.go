package managed

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
)

func TestMutuallyAuthenticatedQUICDiagnosticAndContent(t *testing.T) {
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	defer func() {
		_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
			if err == nil {
				if entry.IsDir() {
					_ = os.Chmod(path, 0o755)
				} else {
					_ = os.Chmod(path, 0o644)
				}
			}
			return nil
		})
	}()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Managed Test\nemail = managed@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	serverRepo, err := repository.Init(ctx, filepath.Join(home, "server"), "server", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer serverRepo.Close()
	clientRepo, err := repository.Join(ctx, serverRepo.Config.Repository, filepath.Join(home, "client"), "client", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer clientRepo.Close()
	filesystemID, err := serverRepo.FileSystemID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, serverRecord := managedTestRecord(t, serverRepo, filesystemID, "quic://127.0.0.1:1")
	_, clientRecord := managedTestRecord(t, clientRepo, filesystemID, "quic://127.0.0.1:1")
	for _, repo := range []*repository.Repository{serverRepo, clientRepo} {
		for _, record := range []membership.Record{serverRecord, clientRecord} {
			if err := membership.Save(repo.Config.Repository, record); err != nil {
				t.Fatal(err)
			}
			if err := membership.Trust(repo.Config.Repository, record.Payload.PeerID, record.Payload.SigningPublicKey); err != nil {
				t.Fatal(err)
			}
		}
	}
	payload := []byte("managed content over quic\n")
	if err := os.WriteFile(filepath.Join(serverRepo.Config.Repository, "payload.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := serverRepo.CommitPending(ctx, "Add managed content"); err != nil {
		t.Fatal(err)
	}
	if err := clientRepo.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := exec.CommandContext(ctx, "git", "-C", serverRepo.Config.Repository, "annex", "lookupkey", "payload.txt").Output()
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 16)
	server, err := Start(serverRepo, "127.0.0.1:0", func(context.Context) ([]byte, error) { return []byte(`{"peer":"server"}`), nil }, nil, nil, nil, func(reason string, _ []string) {
		received <- reason
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	serverRecord.Payload.QUICEndpoint = "quic://" + server.Addr().String()
	serverRecord.Payload.Generation++
	serverRecord.Payload.UpdatedAt = time.Now().UTC()
	serverRecord, err = membership.Sign(serverRecord.Payload, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	serverRecord, err = membership.Approve(serverRecord, serverRepo.Config.PeerID, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []*repository.Repository{serverRepo, clientRepo} {
		if err := membership.Save(repo.Config.Repository, serverRecord); err != nil {
			t.Fatal(err)
		}
	}
	binary := filepath.Join(home, "dfs")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/dfs")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build DFS helper: %v\n%s", err, output)
	}
	if _, err := clientRepo.AdoptClonedPeer(ctx, serverRepo.Config.PeerID); err != nil {
		t.Fatalf("adopt cloned peer: %v", err)
	}
	remoteName, err := clientRepo.AddManagedRemote(ctx, serverRepo.Config.PeerID, binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientRepo.Sync(ctx, true); err != nil {
		t.Fatalf("identify managed annex remote: %v", err)
	}
	if err := clientRepo.ProbeRemote(ctx, remoteName); err != nil {
		t.Fatalf("Git metadata over managed QUIC: %v", err)
	}
	for len(received) > 0 {
		<-received
	}
	push := exec.CommandContext(ctx, "git", "-C", clientRepo.Config.Repository, "push", remoteName, "HEAD:refs/heads/managed-quic-test")
	if output, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push Git metadata over managed QUIC: %v\n%s", err, output)
	}
	select {
	case reason := <-received:
		if reason != "managed Git receive" {
			t.Fatalf("managed Git receive notification = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("managed Git receive did not notify the repository scheduler")
	}
	fakeBin := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	sshMarker := filepath.Join(home, "ssh-invoked")
	if err := os.WriteFile(filepath.Join(fakeBin, "ssh"), []byte("#!/bin/sh\ntouch \"$DFS_SSH_MARKER\"\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+originalPath)
	t.Setenv("DFS_SSH_MARKER", sshMarker)
	offlineRecord := serverRecord
	blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	offlineRecord.Payload.QUICEndpoint = "quic://" + blackhole.LocalAddr().String()
	offlineRecord.Payload.Generation++
	offlineRecord.Payload.UpdatedAt = time.Now().UTC()
	offlineRecord, err = membership.Sign(offlineRecord.Payload, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	offlineRecord, err = membership.Approve(offlineRecord, serverRepo.Config.PeerID, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := membership.Save(clientRepo.Config.Repository, offlineRecord); err != nil {
		t.Fatal(err)
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, 3*time.Second)
	if err := clientRepo.ProbeRemote(probeCtx, remoteName); err == nil {
		cancelProbe()
		t.Fatal("offline QUIC remote unexpectedly succeeded")
	}
	cancelProbe()
	if _, err := os.Stat(sshMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline QUIC probe invoked SSH: %v", err)
	}
	if err := membership.Save(clientRepo.Config.Repository, serverRecord); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", originalPath)
	connection, stream, reader, response, err := Open(ctx, clientRepo, serverRepo.Config.PeerID, Request{Operation: "diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := make([]byte, response.Size)
	if _, err := io.ReadFull(reader, diagnostic); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	_ = connection.CloseWithError(0, "")
	if string(diagnostic) != `{"peer":"server"}` {
		t.Fatalf("diagnostic = %s", diagnostic)
	}
	var gitOutput bytes.Buffer
	mode, err := GitProxy(ctx, clientRepo, serverRepo.Config.PeerID, "git-upload-pack", bytes.NewReader(nil), &gitOutput, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "quic" || !bytes.Contains(gitOutput.Bytes(), []byte("HEAD")) {
		t.Fatalf("managed Git mode=%q output=%q", mode, gitOutput.Bytes())
	}
	connection, stream, reader, response, err = Open(ctx, clientRepo, serverRepo.Config.PeerID, Request{Operation: "annex-get", Key: strings.TrimSpace(string(keyBytes))})
	if err != nil {
		t.Fatal(err)
	}
	downloaded := make([]byte, response.Size)
	if _, err := io.ReadFull(reader, downloaded); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	_ = connection.CloseWithError(0, "")
	if string(downloaded) != string(payload) {
		t.Fatalf("downloaded content = %q", downloaded)
	}
	// A dead trusted peer is skipped and the same authenticated range is served
	// by the next available peer without exposing partial output to the caller.
	_, unavailablePrivate, unavailableErr := ed25519.GenerateKey(nil)
	if unavailableErr != nil {
		t.Fatal(unavailableErr)
	}
	unavailablePublic := unavailablePrivate.Public().(ed25519.PublicKey)
	unavailablePayload := serverRecord.Payload
	unavailablePayload.PeerID = "dfs-peer-000000000000"
	unavailablePayload.Name = "unavailable"
	unavailablePayload.Hostname = "unavailable"
	unavailablePayload.SigningPublicKey = base64Public(unavailablePublic)
	unavailablePayload.QUICEndpoint = "quic://" + blackhole.LocalAddr().String()
	unavailablePayload.Generation = 1
	unavailablePayload.UpdatedAt = time.Now().UTC()
	unavailableRecord, err := membership.Sign(unavailablePayload, unavailablePrivate)
	if err != nil {
		t.Fatal(err)
	}
	unavailableRecord, err = membership.Approve(unavailableRecord, serverRepo.Config.PeerID, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := membership.Save(clientRepo.Config.Repository, unavailableRecord); err != nil {
		t.Fatal(err)
	}
	if err := membership.Trust(clientRepo.Config.Repository, unavailableRecord.Payload.PeerID, unavailableRecord.Payload.SigningPublicKey); err != nil {
		t.Fatal(err)
	}
	var ranged bytes.Buffer
	total, err := FetchRange(ctx, clientRepo, strings.TrimSpace(string(keyBytes)), 8, 7, &ranged)
	if err != nil {
		t.Fatalf("range fetch with failed first peer: %v", err)
	}
	if total != int64(len(payload)) || ranged.String() != string(payload[8:15]) {
		t.Fatalf("range fetch = total %d, data %q", total, ranged.String())
	}

	clientRepo.SetManagedRangeFetcher(FetchRange)
	streamed := make([]byte, len(payload))
	if n, err := clientRepo.ReadRange(ctx, "payload.txt", strings.TrimSpace(string(keyBytes)), int64(len(payload)), 0, streamed); err != nil || n != len(payload) {
		t.Fatalf("stream and promote complete content = %d, %v", n, err)
	}
	if !bytes.Equal(streamed, payload) {
		t.Fatalf("streamed content = %q", streamed)
	}
	if output, err := exec.CommandContext(ctx, "git", "-C", clientRepo.Config.Repository, "annex", "find", "--in=here", "--format=${file}", "--", "payload.txt").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "payload.txt" {
		t.Fatalf("verified range was not promoted: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(ctx, "git", "-C", clientRepo.Config.Repository, "annex", "drop", "--force", "--", "payload.txt").CombinedOutput(); err != nil {
		t.Fatalf("drop streamed content before whole-fetch compatibility test: %v\n%s", err, output)
	}
	clientRepo.SetManagedFetcher(FetchPath)
	if err := clientRepo.Fetch(ctx, "payload.txt", ""); err != nil {
		t.Fatal(err)
	}
	materialized, err := os.ReadFile(filepath.Join(clientRepo.Config.Repository, "payload.txt"))
	if err != nil || string(materialized) != string(payload) {
		t.Fatalf("managed repository fetch = %q, %v", materialized, err)
	}
	if output, err := exec.CommandContext(ctx, "git", "-C", clientRepo.Config.Repository, "annex", "drop", "--force", "--", "payload.txt").CombinedOutput(); err != nil {
		t.Fatalf("drop content before offline QUIC test: %v\n%s", err, output)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+originalPath)
	if err := membership.Save(clientRepo.Config.Repository, offlineRecord); err != nil {
		t.Fatal(err)
	}
	fetchCtx, cancelFetch := context.WithTimeout(ctx, 3*time.Second)
	if err := clientRepo.Fetch(fetchCtx, "payload.txt", remoteName); err == nil {
		cancelFetch()
		t.Fatal("content fetch from offline QUIC peer unexpectedly succeeded")
	}
	cancelFetch()
	if _, err := os.Stat(sshMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline QUIC content fetch invoked SSH: %v", err)
	}
}

func TestLocalIPv4PrefersIPv4ForLocalHostname(t *testing.T) {
	lookupCalled := false
	lookup := func(_ context.Context, network, hostname string) ([]net.IP, error) {
		lookupCalled = true
		if network != "ip4" || hostname != "peer.local" {
			t.Fatalf("lookup = %q, %q", network, hostname)
		}
		return []net.IP{net.ParseIP("192.0.2.10")}, nil
	}
	if address, ok := localIPv4WithResolver(context.Background(), "Peer.Local.", lookup); !ok || address != "192.0.2.10" {
		t.Fatalf("local IPv4 = %q, %v", address, ok)
	}
	if !lookupCalled {
		t.Fatal("local hostname was not resolved")
	}
	lookupCalled = false
	if address, ok := localIPv4WithResolver(context.Background(), "example.com", lookup); ok || address != "" {
		t.Fatalf("non-local IPv4 = %q, %v", address, ok)
	}
	if lookupCalled {
		t.Fatal("non-local hostname unexpectedly resolved")
	}
}

func managedTestRecord(t *testing.T, repo *repository.Repository, filesystemID, endpoint string) (ed25519.PrivateKey, membership.Record) {
	t.Helper()
	key, public, err := membership.EnsureKey(repo.Config.Repository)
	if err != nil {
		t.Fatal(err)
	}
	record, err := membership.Sign(membership.Payload{Version: membership.Version, FileSystemID: filesystemID,
		PeerID: repo.Config.PeerID, Name: repo.Config.Name, Hostname: repo.Config.Hostname, Role: "admin", SigningPublicKey: public,
		SSH:          membership.SSHTransport{Endpoint: fmt.Sprintf("ssh://user@%s.local%s", repo.Config.Hostname, repo.Config.Repository), PublicKey: "ssh-ed25519 test"},
		QUICEndpoint: endpoint, Generation: 1, UpdatedAt: time.Now().UTC()}, key)
	if err != nil {
		t.Fatal(err)
	}
	record, err = membership.Approve(record, repo.Config.PeerID, key)
	if err != nil {
		t.Fatal(err)
	}
	return key, record
}
