package membership

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcceptedFollowsSignedApprovalChain(t *testing.T) {
	shared := gitMembershipRepository(t)
	filesystemID := strings.Repeat("a", 40)
	aliceKey, alicePublic, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bobKey, bobPublic, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	charlieKey, charliePublic, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alice := signedTestRecord(t, filesystemID, "alice-peer", "admin", alicePublic, aliceKey)
	alice, err = Approve(alice, "alice-peer", aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	bob := signedTestRecord(t, filesystemID, "bob-peer", "member", bobPublic, bobKey)
	bob, err = Approve(bob, "alice-peer", aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	charlie := signedTestRecord(t, filesystemID, "charlie-peer", "member", charliePublic, charlieKey)
	charlie, err = Approve(charlie, "charlie-peer", charlieKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []Record{alice, bob, charlie} {
		if err := Save(shared, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := Trust(shared, alice.Payload.PeerID, alice.Payload.SigningPublicKey); err != nil {
		t.Fatal(err)
	}
	accepted, err := Accepted(shared, filesystemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 2 || accepted[0].Payload.PeerID != "alice-peer" || accepted[1].Payload.PeerID != "bob-peer" {
		t.Fatalf("accepted membership = %#v", accepted)
	}
	revocation, err := Revoke(filesystemID, "bob-peer", "alice-peer", 2, aliceKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRevocation(shared, revocation); err != nil {
		t.Fatal(err)
	}
	accepted, err = Accepted(shared, filesystemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 1 || accepted[0].Payload.PeerID != "alice-peer" {
		t.Fatalf("membership after revocation = %#v", accepted)
	}
	if err := removeSharedFile(shared, revocationsPrefix+"bob-peer.json"); err != nil {
		t.Fatal(err)
	}
	accepted, err = Accepted(shared, filesystemID)
	if err != nil || len(accepted) != 1 || accepted[0].Payload.PeerID != "alice-peer" {
		t.Fatalf("locally persisted revocation was undone: %#v, %v", accepted, err)
	}
}

func TestLegacyMembershipMovesOutOfWorktree(t *testing.T) {
	repositoryPath := gitMembershipRepository(t)
	key, public, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := signedTestRecord(t, strings.Repeat("d", 40), "alice-peer", "admin", public, key)
	record, err = Approve(record, "alice-peer", key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(repositoryPath, ".dfs", "members")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "alice-peer.json"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	attributes := "*.bin annex.largefiles=anything\n.dfs/members/** annex.largefiles=nothing\n.dfs/revocations/** annex.largefiles=nothing\n"
	if err := os.WriteFile(filepath.Join(repositoryPath, ".gitattributes"), []byte(attributes), 0o644); err != nil {
		t.Fatal(err)
	}
	gitMembershipRun(t, repositoryPath, "add", ".dfs/members/alice-peer.json", ".gitattributes")
	gitMembershipRun(t, repositoryPath, "commit", "-m", "Legacy membership")
	if err := MigrateLegacySharedState(repositoryPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repositoryPath, ".dfs", "members")); !os.IsNotExist(err) {
		t.Fatalf("legacy membership remains visible: %v", err)
	}
	remaining, err := os.ReadFile(filepath.Join(repositoryPath, ".gitattributes"))
	if err != nil || string(remaining) != "*.bin annex.largefiles=anything\n" {
		t.Fatalf("retained attributes = %q, %v", remaining, err)
	}
	records, err := LoadAll(repositoryPath)
	if err != nil || len(records) != 1 || records[0].Payload.PeerID != "alice-peer" {
		t.Fatalf("migrated records = %#v, %v", records, err)
	}
	if output, err := exec.Command("git", "-C", repositoryPath, "ls-tree", "-r", "--name-only", "HEAD").Output(); err != nil || strings.Contains(string(output), ".dfs/") {
		t.Fatalf("worktree still tracks DFS membership: %s, %v", output, err)
	}
}

func TestMembershipRefSynchronizesWithoutWorktreeFiles(t *testing.T) {
	first := gitMembershipRepository(t)
	second := filepath.Join(t.TempDir(), "second")
	if output, err := exec.Command("git", "clone", first, second).CombinedOutput(); err != nil {
		t.Fatalf("clone second repository: %v\n%s", err, output)
	}
	gitMembershipRun(t, second, "config", "user.name", "Membership Test")
	gitMembershipRun(t, second, "config", "user.email", "membership@example.invalid")
	gitMembershipRun(t, first, "remote", "add", "second", second)
	gitMembershipRun(t, second, "remote", "add", "first", first)
	filesystemID := strings.Repeat("e", 40)
	firstKey, firstPublic, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secondKey, secondPublic, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstRecord := signedTestRecord(t, filesystemID, "first-peer", "admin", firstPublic, firstKey)
	firstRecord, err = Approve(firstRecord, "first-peer", firstKey)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord := signedTestRecord(t, filesystemID, "second-peer", "member", secondPublic, secondKey)
	secondRecord, err = Approve(secondRecord, "first-peer", firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(first, firstRecord); err != nil {
		t.Fatal(err)
	}
	if err := Save(second, secondRecord); err != nil {
		t.Fatal(err)
	}
	if err := Trust(first, "first-peer", firstPublic); err != nil {
		t.Fatal(err)
	}
	if err := Trust(second, "first-peer", firstPublic); err != nil {
		t.Fatal(err)
	}
	if err := Sync(context.Background(), first, []string{"second"}); err != nil {
		t.Fatal(err)
	}
	if err := Sync(context.Background(), second, []string{"first"}); err != nil {
		t.Fatal(err)
	}
	for _, repositoryPath := range []string{first, second} {
		records, err := LoadAll(repositoryPath)
		if err != nil || len(records) != 2 {
			t.Fatalf("records in %s = %#v, %v", repositoryPath, records, err)
		}
		if _, err := os.Stat(filepath.Join(repositoryPath, ".dfs")); !os.IsNotExist(err) {
			t.Fatalf("membership leaked into worktree %s: %v", repositoryPath, err)
		}
	}
}

func TestSignedClusterPinPolicyReplicatesAndUnpins(t *testing.T) {
	first := gitMembershipRepository(t)
	second := filepath.Join(t.TempDir(), "second")
	if output, err := exec.Command("git", "clone", first, second).CombinedOutput(); err != nil {
		t.Fatalf("clone second repository: %v\n%s", err, output)
	}
	gitMembershipRun(t, second, "config", "user.name", "Membership Test")
	gitMembershipRun(t, second, "config", "user.email", "membership@example.invalid")
	gitMembershipRun(t, first, "remote", "add", "second", second)
	gitMembershipRun(t, second, "remote", "add", "first", first)
	filesystemID := strings.Repeat("f", 40)
	firstKey, firstPublic, err := EnsureKey(first)
	if err != nil {
		t.Fatal(err)
	}
	secondKey, secondPublic, err := EnsureKey(second)
	if err != nil {
		t.Fatal(err)
	}
	firstRecord := signedTestRecord(t, filesystemID, "first-peer", "admin", firstPublic, firstKey)
	firstRecord, err = Approve(firstRecord, "first-peer", firstKey)
	if err != nil {
		t.Fatal(err)
	}
	secondRecord := signedTestRecord(t, filesystemID, "second-peer", "member", secondPublic, secondKey)
	secondRecord, err = Approve(secondRecord, "first-peer", firstKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, repositoryPath := range []string{first, second} {
		for _, record := range []Record{firstRecord, secondRecord} {
			if err := Save(repositoryPath, record); err != nil {
				t.Fatal(err)
			}
			if err := Trust(repositoryPath, record.Payload.PeerID, record.Payload.SigningPublicKey); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := SetPinPolicy(first, filesystemID, "first-peer", "Media/Films", true); err != nil {
		t.Fatal(err)
	}
	if err := Sync(context.Background(), first, []string{"second"}); err != nil {
		t.Fatal(err)
	}
	if paths, err := ActivePinPaths(second, filesystemID); err != nil || len(paths) != 1 || paths[0] != "Media/Films" {
		t.Fatalf("replicated active pins = %v, %v", paths, err)
	}
	policy, err := SetPinPolicy(second, filesystemID, "second-peer", "Media/Films", false)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Generation != 2 {
		t.Fatalf("unpin generation = %d, want 2", policy.Generation)
	}
	if err := Sync(context.Background(), second, []string{"first"}); err != nil {
		t.Fatal(err)
	}
	if paths, err := ActivePinPaths(first, filesystemID); err != nil || len(paths) != 0 {
		t.Fatalf("active pins after replicated unpin = %v, %v", paths, err)
	}
	for _, repositoryPath := range []string{first, second} {
		if output := gitMembershipRunOutput(t, repositoryPath, "ls-tree", "-r", "--name-only", "HEAD"); strings.Contains(output, "pins/") {
			t.Fatalf("cluster pin leaked into worktree history in %s: %s", repositoryPath, output)
		}
		if output := gitMembershipRunOutput(t, repositoryPath, "ls-tree", "-r", "--name-only", PinRef); !strings.Contains(output, "pins/") {
			t.Fatalf("cluster pin missing from dedicated metadata ref in %s: %s", repositoryPath, output)
		}
	}
	files, err := loadSharedPrefix(first, PinRef, pinsPrefix)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range files {
		var tampered PinPolicy
		if err := json.Unmarshal(data, &tampered); err != nil {
			t.Fatal(err)
		}
		tampered.Pinned = true
		changed, err := json.Marshal(tampered)
		if err != nil {
			t.Fatal(err)
		}
		if err := writePinPolicyFile(first, path, changed); err != nil {
			t.Fatal(err)
		}
	}
	if paths, err := ActivePinPaths(first, filesystemID); err != nil || len(paths) != 0 {
		t.Fatalf("tampered cluster pin policy was accepted: %v, %v", paths, err)
	}
}

func TestMembershipRejectsTamperingAndKeyReplacement(t *testing.T) {
	repositoryPath := t.TempDir()
	key, public, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := signedTestRecord(t, strings.Repeat("b", 40), "alice-peer", "admin", public, key)
	record, err = Approve(record, "alice-peer", key)
	if err != nil {
		t.Fatal(err)
	}
	tampered := record
	tampered.Payload.Name = "mallory"
	if err := VerifySelf(tampered); err == nil {
		t.Fatal("tampered membership signature was accepted")
	}
	if err := Trust(repositoryPath, record.Payload.PeerID, public); err != nil {
		t.Fatal(err)
	}
	_, replacement, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := Trust(repositoryPath, record.Payload.PeerID, replacement); err == nil {
		t.Fatal("trusted membership key replacement was accepted")
	}
}

func TestSignedEndorsement(t *testing.T) {
	key, public, err := EnsureKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := signedTestRecord(t, strings.Repeat("c", 40), "alice-peer", "admin", public, key)
	record, err = Approve(record, "alice-peer", key)
	if err != nil {
		t.Fatal(err)
	}
	endorsement, err := Endorse(record, "alice-peer", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEndorsement(endorsement, record); err != nil {
		t.Fatal(err)
	}
	endorsement.PeerID = "mallory-peer"
	if err := VerifyEndorsement(endorsement, record); err == nil {
		t.Fatal("tampered endorsement was accepted")
	}
}

func signedTestRecord(t *testing.T, filesystemID, peerID, role, public string, key ed25519.PrivateKey) Record {
	t.Helper()
	record, err := Sign(Payload{Version: Version, FileSystemID: filesystemID, PeerID: peerID, Name: peerID,
		Hostname: peerID, Role: role, SigningPublicKey: public, SSH: SSHTransport{Endpoint: "ssh://user@" + peerID + ".local/repository", PublicKey: "ssh-ed25519 test"}, QUICEndpoint: "quic://" + peerID + ".local:7843",
		Generation: 1, UpdatedAt: time.Now().UTC()}, key)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func gitMembershipRepository(t *testing.T) string {
	t.Helper()
	repositoryPath := t.TempDir()
	gitMembershipRun(t, repositoryPath, "init", "-b", "main")
	gitMembershipRun(t, repositoryPath, "config", "user.name", "Membership Test")
	gitMembershipRun(t, repositoryPath, "config", "user.email", "membership@example.invalid")
	gitMembershipRun(t, repositoryPath, "commit", "--allow-empty", "-m", "Initialize")
	return repositoryPath
}

func gitMembershipRun(t *testing.T, repositoryPath string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repositoryPath}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func gitMembershipRunOutput(t *testing.T, repositoryPath string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repositoryPath}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
