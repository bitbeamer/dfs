package managed

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
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
	server, err := Start(serverRepo, "127.0.0.1:0", func(context.Context) ([]byte, error) { return []byte(`{"peer":"server"}`), nil }, nil, nil)
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
	remoteName, err := clientRepo.AddManagedRemote(ctx, serverRepo.Config.PeerID, binary, serverRecord.Payload.SSH.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientRepo.ProbeRemote(ctx, remoteName); err != nil {
		t.Fatalf("Git metadata over managed QUIC: %v", err)
	}
	push := exec.CommandContext(ctx, "git", "-C", clientRepo.Config.Repository, "push", remoteName, "HEAD:refs/heads/managed-quic-test")
	if output, err := push.CombinedOutput(); err != nil {
		t.Fatalf("push Git metadata over managed QUIC: %v\n%s", err, output)
	}
	fakeBin := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "ssh"), []byte("#!/bin/sh\nfor last do :; done\nexec sh -c \"$last\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+originalPath)
	offlineRecord := serverRecord
	offlineRecord.Payload.QUICEndpoint = "quic://127.0.0.1:1"
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
	if err := clientRepo.ProbeRemote(ctx, remoteName); err != nil {
		t.Fatalf("Git metadata over SSH fallback: %v", err)
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
	clientRepo.SetManagedFetcher(FetchPath)
	if err := clientRepo.Fetch(ctx, "payload.txt", ""); err != nil {
		t.Fatal(err)
	}
	materialized, err := os.ReadFile(filepath.Join(clientRepo.Config.Repository, "payload.txt"))
	if err != nil || string(materialized) != string(payload) {
		t.Fatalf("managed repository fetch = %q, %v", materialized, err)
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
