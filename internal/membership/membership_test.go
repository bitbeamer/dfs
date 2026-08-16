package membership

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcceptedFollowsSignedApprovalChain(t *testing.T) {
	shared := t.TempDir()
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
	if err := os.Remove(filepath.Join(shared, ".dfs", "revocations", "bob-peer.json")); err != nil {
		t.Fatal(err)
	}
	accepted, err = Accepted(shared, filesystemID)
	if err != nil || len(accepted) != 1 || accepted[0].Payload.PeerID != "alice-peer" {
		t.Fatalf("locally persisted revocation was undone: %#v, %v", accepted, err)
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
