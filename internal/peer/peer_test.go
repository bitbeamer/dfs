package peer

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/membership"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/pion/mdns/v2"
)

func TestInvitationRoundTripAndValidation(t *testing.T) {
	invitation := Invitation{
		Version: ProtocolVersion, FileSystemID: strings.Repeat("a", 40), InvitationID: "invite",
		Secret: "secret", CertificateSHA256: strings.Repeat("b", 64), QUICEndpoint: "quic://desktop.local:1234",
	}
	encoded, err := invitation.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeInvitation(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != invitation {
		t.Fatalf("decoded invitation = %#v, want %#v", decoded, invitation)
	}
	if _, err := DecodeInvitation("not-an-invitation"); err == nil {
		t.Fatal("invalid invitation unexpectedly decoded")
	}
}

func TestOfferFromEvent(t *testing.T) {
	event := mdns.ServiceEvent{
		Addr: netip.MustParseAddr("192.0.2.10"),
		Instance: mdns.ServiceInstance{Host: "desktop.local.", Port: 44123, Text: pionTXTEntries([]string{
			"v=1", "fs=" + strings.Repeat("a", 40), "network=" + encodeTXT("Home Files"),
			"peer=0123456789abcdef", "name=" + encodeTXT("Desktop"), "cert=" + strings.Repeat("b", 64),
		})},
	}
	offer, found := offerFromEvent(event)
	if !found {
		t.Fatal("valid event did not produce an offer")
	}
	if offer.NetworkName != "Home Files" || offer.PeerName != "Desktop" || offer.Endpoint != "quic://192.0.2.10:44123" {
		t.Fatalf("offer = %#v", offer)
	}

	event.Instance.Text = pionTXTEntries([]string{"v=broken"})
	if offer, found := offerFromEvent(event); found {
		t.Fatalf("invalid event produced offer: %#v", offer)
	}
	networks := GroupOffers([]Offer{
		{FileSystemID: "same", NetworkName: "Home", PeerName: "desktop"},
		{FileSystemID: "same", NetworkName: "Home", PeerName: "laptop"},
		{FileSystemID: "other", NetworkName: "Archive", PeerName: "server"},
	})
	if len(networks) != 2 || networks[0].NetworkName != "Archive" || len(networks[1].Offers) != 2 {
		t.Fatalf("grouped networks = %#v", networks)
	}
}

func TestReachableOffersDropsStaleAdvertisements(t *testing.T) {
	offers := []Offer{
		{PeerName: "stale", Endpoint: "quic://192.0.2.1:7843", ProtocolVersion: ProtocolVersion, CertificateSHA256: "stale-cert"},
		{PeerName: "online", Endpoint: "quic://192.0.2.2:7843", ProtocolVersion: ProtocolVersion, CertificateSHA256: "online-cert"},
		{PeerName: "incompatible", Endpoint: "quic://192.0.2.3:7843", ProtocolVersion: ProtocolVersion + 1, CertificateSHA256: "other-cert"},
	}
	probed := make(chan string, len(offers))
	got := reachableOffers(context.Background(), offers, time.Second, func(_ context.Context, endpoint, _ string) error {
		probed <- endpoint
		if endpoint == offers[1].Endpoint {
			return nil
		}
		return errors.New("offline")
	})
	if len(got) != 1 || got[0].PeerName != "online" {
		t.Fatalf("reachable offers = %#v, want only online peer", got)
	}
	if len(probed) != 2 {
		t.Fatalf("probed %d compatible offers, want 2", len(probed))
	}
}

func TestMeshDiscoveryCannotIntroduceUnacceptedPeer(t *testing.T) {
	filesystemID := strings.Repeat("a", 40)
	peers := map[string]MeshPeer{
		"accepted-peer": {PeerID: "accepted-peer", PeerName: "accepted"},
	}
	markDiscoveredAcceptedPeers(peers, []Offer{
		{FileSystemID: filesystemID, PeerID: "accepted-peer"},
		{FileSystemID: filesystemID, PeerID: "unknown-peer"},
	}, filesystemID, "local-peer")
	if !peers["accepted-peer"].Online {
		t.Fatal("accepted discovered peer was not marked online")
	}
	if _, found := peers["unknown-peer"]; found {
		t.Fatal("unauthenticated discovery introduced an unknown mesh peer")
	}
}

func TestValidatePairingMembershipRejectsAdminClaim(t *testing.T) {
	filesystemID := strings.Repeat("a", 40)
	private, draft, err := newMembershipDraft(filepath.Join(t.TempDir(), "draft"), filesystemID, "joining-peer", "joining", 7843)
	if err != nil {
		t.Fatal(err)
	}
	draft.Payload.Role = "admin"
	draft, err = membership.Sign(draft.Payload, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := validatePairingMembership(draft, filesystemID, "joining-peer", "joining"); err == nil {
		t.Fatal("pairing accepted an administrator role claim")
	}
}

func TestMDNSHostname(t *testing.T) {
	for input, expected := range map[string]string{
		"cachyos":     "cachyos.local.",
		"zeus.local":  "zeus.local.",
		"ZEUS.LOCAL.": "ZEUS.LOCAL.",
	} {
		if actual := mdnsHostname(input); actual != expected {
			t.Errorf("mdnsHostname(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestServiceAdvertisesWithMDNS(t *testing.T) {
	if os.Getenv("DFS_TEST_MDNS") != "1" {
		t.Skip("set DFS_TEST_MDNS=1 to exercise multicast discovery")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"git-annex"} {
		if err := os.WriteFile(filepath.Join(bin, command), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Discovery Test\nemail = discovery@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, err := repository.Init(ctx, filepath.Join(home, "advertised"), "desktop", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	repo.Config.NetworkName = "Discovered Files"
	if err := repo.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	testInterface := os.Getenv("DFS_TEST_MDNS_INTERFACE")
	var loopback *net.Interface
	if testInterface != "" {
		loopback, err = net.InterfaceByName(testInterface)
	} else {
		interfaces, listErr := net.Interfaces()
		err = listErr
		for index := range interfaces {
			if interfaces[index].Flags&net.FlagLoopback != 0 {
				loopback = &interfaces[index]
				break
			}
		}
		if err == nil && loopback == nil {
			err = errors.New("no loopback interface")
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	previousProvider := interfaceProvider
	interfaceProvider = func() []*net.Interface { return []*net.Interface{loopback} }
	defer func() { interfaceProvider = previousProvider }()
	service, err := Start(repo, nil, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	offers, err := Discover(ctx, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, offer := range offers {
		if offer.FileSystemID == service.filesystemID && offer.NetworkName == "Discovered Files" && offer.PeerName == "desktop" {
			return
		}
	}
	t.Fatalf("advertised DFS network not discovered: %#v", offers)
}

func TestPairAndJoinConfiguresBothPeers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"git-annex"} {
		if err := os.WriteFile(filepath.Join(bin, command), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Pair Test\nemail = pair@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	existing, err := repository.Init(ctx, filepath.Join(home, "existing"), "desktop", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	existing.Config.NetworkName = "Home Files"
	if err := existing.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service, err := startService(existing, nil, listener, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	invitation, err := CreateInvitation(existing, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	invitation.QUICEndpoint = "quic://" + listener.Addr().String()
	encoded, err := invitation.Encode()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(home, "laptop")
	var progress []string
	result, err := PairAndJoinWithOptions(ctx, encoded, destination, "laptop", 5<<20, 20*time.Millisecond, true, PairOptions{
		Progress: func(message string) { progress = append(progress, message) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Repository.Close()
	if result.NetworkName != "Home Files" || result.OfferingPeer != "desktop" {
		t.Fatalf("join result = %#v", result)
	}
	if result.ReverseRemoteName == "" {
		t.Fatal("pairing did not configure a reverse remote")
	}
	for _, wanted := range []string{"Locating the approving DFS peer", "Approval confirmed by desktop", "Repository downloaded", "Completing reciprocal DFS pairing with desktop", "Reciprocal pairing completed"} {
		if !slices.ContainsFunc(progress, func(message string) bool { return strings.Contains(message, wanted) }) {
			t.Fatalf("pairing progress %#v does not contain %q", progress, wanted)
		}
	}
	existingID, err := existing.FileSystemID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	joinedID, err := result.Repository.FileSystemID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if joinedID != existingID || result.Repository.Config.PeerID == existing.Config.PeerID {
		t.Fatalf("paired identities existing=%q joined=%q peers=%q/%q", existingID, joinedID, existing.Config.PeerID, result.Repository.Config.PeerID)
	}
	if _, record, err := ensureLocalMembership(ctx, result.Repository, joinedID, DefaultPairingPort); err != nil {
		t.Fatalf("restart paired member service: %v", err)
	} else if record.Payload.Role != "member" {
		t.Fatalf("restarted paired role = %q, want member", record.Payload.Role)
	}
	remotes, err := existing.Remotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var foundReverse bool
	for _, remote := range remotes {
		if remote.Name == result.ReverseRemoteName && strings.HasPrefix(remote.URL, "ext::") {
			foundReverse = true
		}
	}
	if !foundReverse {
		t.Fatalf("reverse remote %q not present in %#v", result.ReverseRemoteName, remotes)
	}
	joinedRemotes, err := result.Repository.Remotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expectedSource := "dfs-peer-" + existing.Config.PeerID[:12]
	if len(joinedRemotes) != 1 || joinedRemotes[0].Name != expectedSource {
		t.Fatalf("joined remotes = %#v, want paired source %q", joinedRemotes, expectedSource)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "authorized_keys")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("QUIC pairing modified authorized_keys: %v", err)
	}
	for _, repo := range []*repository.Repository{existing, result.Repository} {
		command := exec.Command("git", "-C", repo.Config.Repository, "config", "--get", "core.sshCommand")
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("QUIC pairing configured core.sshCommand = %q", output)
		}
		if _, err := os.Stat(filepath.Join(repo.Config.Repository, ".git", "dfs", "peer-ssh-key")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("QUIC pairing created an SSH transport key: %v", err)
		}
		records, err := membership.LoadAll(repo.Config.Repository)
		if err != nil || len(records) != 2 {
			t.Fatalf("paired membership records = %#v, %v", records, err)
		}
	}
	secondInvitation, err := CreateInvitation(existing, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secondInvitation.QUICEndpoint = "quic://" + listener.Addr().String()
	secondEncoded, err := secondInvitation.Encode()
	if err != nil {
		t.Fatal(err)
	}
	thirdDestination := filepath.Join(home, "tablet")
	third, err := PairAndJoin(ctx, secondEncoded, thirdDestination, "tablet", 5<<20, 20*time.Millisecond, true)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Repository.Close()
	thirdRemotes, err := third.Repository.Remotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(thirdRemotes) != 2 {
		t.Fatalf("third peer did not reconcile the existing mesh: %#v", thirdRemotes)
	}
	active, err := ListInvitations(existing.Config.Repository, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("completed invitation remains active: %#v", active)
	}
}

func TestServiceCloseWaitsForBackgroundWork(t *testing.T) {
	repositoryPath := t.TempDir()
	cleanupDone := make(chan struct{})
	close(cleanupDone)
	service := &Service{
		repo:        &repository.Repository{Config: config.Default("test", repositoryPath)},
		cleanupStop: make(chan struct{}), cleanupDone: cleanupDone,
	}
	service.runBackground = service.startBackground
	started := make(chan struct{})
	release := make(chan struct{})
	service.runBackground(func() {
		close(started)
		<-release
	})
	<-started
	closed := make(chan error, 1)
	go func() { closed <- service.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before background work completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after background work completed")
	}
}

func TestPairAndJoinSupersedesPurgedAndRejoinedPeer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git-annex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Rejoin Test\nemail = rejoin@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	existing, err := repository.Init(ctx, filepath.Join(home, "ares"), "ares", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service, err := startService(existing, nil, listener, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.runBackground = func(func()) {}

	destination := filepath.Join(home, "zeus")
	joinZeus := func() *JoinResult {
		t.Helper()
		invitation, err := CreateInvitation(existing, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		invitation.QUICEndpoint = "quic://" + listener.Addr().String()
		encoded, err := invitation.Encode()
		if err != nil {
			t.Fatal(err)
		}
		result, err := PairAndJoin(ctx, encoded, destination, "zeus", 5<<20, 20*time.Millisecond, true)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := joinZeus()
	oldPeerID := first.Repository.Config.PeerID
	if err := first.Repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(destination); err != nil {
		t.Fatal(err)
	}

	second := joinZeus()
	defer second.Repository.Close()
	newPeerID := second.Repository.Config.PeerID
	if newPeerID == oldPeerID {
		t.Fatalf("purged Zeus reused peer identity %q", oldPeerID)
	}
	if err := ConfigureMembership(ctx, existing); err != nil {
		t.Fatal(err)
	}

	remotes, err := existing.Remotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldRemote := remoteName(oldPeerID)
	newRemote := remoteName(newPeerID)
	if slices.ContainsFunc(remotes, func(remote repository.Remote) bool { return remote.Name == oldRemote }) {
		t.Fatalf("purged Zeus remote %q remains configured: %#v", oldRemote, remotes)
	}
	if !slices.ContainsFunc(remotes, func(remote repository.Remote) bool { return remote.Name == newRemote }) {
		t.Fatalf("rejoined Zeus remote %q is not configured: %#v", newRemote, remotes)
	}
	filesystemID, err := existing.FileSystemID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := acceptedMembership(ctx, existing, filesystemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 2 || !slices.ContainsFunc(accepted, func(record membership.Record) bool {
		return record.Payload.Name == "zeus" && record.Payload.PeerID == newPeerID
	}) {
		t.Fatalf("active membership after Zeus purge and rejoin = %#v", accepted)
	}
}

func TestVerifyMembershipApprovalChainAcceptsNonFounderApprover(t *testing.T) {
	filesystemID := strings.Repeat("a", 40)
	rootKey, root, err := newMembershipDraft(filepath.Join(t.TempDir(), "root"), filesystemID, "root-peer", "root", 7843)
	if err != nil {
		t.Fatal(err)
	}
	root.Payload.Role = "admin"
	root, err = membership.Sign(root.Payload, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = membership.Approve(root, root.Payload.PeerID, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	approverKey, approver, err := newMembershipDraft(filepath.Join(t.TempDir(), "approver"), filesystemID, "approver-peer", "approver", 7844)
	if err != nil {
		t.Fatal(err)
	}
	approver.Payload.Role = "admin"
	approver, err = membership.Sign(approver.Payload, approverKey)
	if err != nil {
		t.Fatal(err)
	}
	approver, err = membership.Approve(approver, root.Payload.PeerID, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyMembershipApprovalChain(approver, []membership.Record{root, approver}, filesystemID); err != nil {
		t.Fatalf("valid non-founder approval chain rejected: %v", err)
	}
	if bootstrap, err := membershipApprovalRoot(approver, []membership.Record{root, approver}, filesystemID); err != nil || bootstrap.Payload.PeerID != root.Payload.PeerID {
		t.Fatalf("approval root = %#v, %v", bootstrap, err)
	}
	if err := verifyMembershipApprovalChain(approver, []membership.Record{approver}, filesystemID); err == nil || !strings.Contains(err.Error(), "missing peer") {
		t.Fatalf("incomplete approval chain error = %v", err)
	}
}

func TestDiscoveredJoinRequestRequiresExplicitBoundApproval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git-annex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Join Test\nemail = join@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo, err := repository.Init(ctx, filepath.Join(home, "source"), "source", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service, err := startService(repo, nil, listener, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	filesystemID, err := repo.FileSystemID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	network := Network{FileSystemID: filesystemID, NetworkName: "Join Test", Offers: []Offer{{
		FileSystemID: filesystemID, NetworkName: "Join Test", PeerID: repo.Config.PeerID, PeerName: repo.Config.Name,
		Endpoint: "quic://" + listener.Addr().String(), ProtocolVersion: ProtocolVersion, CertificateSHA256: service.fingerprint,
	}}}
	joiningID := "abcdef0123456789"
	stateDirectory := filepath.Join(home, "join-state")
	credentials, err := SubmitJoinRequest(ctx, network, joiningID, "joining", stateDirectory, 7844, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(credentials.ApprovingPeers, []string{repo.Config.Name}) {
		t.Fatalf("join request approving peers = %#v", credentials.ApprovingPeers)
	}
	data, err := os.ReadFile(joinRequestPath(repo.Config.Repository, credentials.RequestID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), credentials.Secret) {
		t.Fatal("persisted join request contains its bearer secret")
	}
	if _, err := ApproveJoinRequest(repo, "missing-request", time.Minute); err == nil || !strings.Contains(err.Error(), "not pending on this peer") {
		t.Fatalf("missing local join request error = %v", err)
	}
	if invitation, approved, err := PollJoinApproval(ctx, network, credentials); err != nil || approved {
		t.Fatalf("unapproved request = %#v, %v, %v", invitation, approved, err)
	}
	invitation, err := ApproveJoinRequest(repo, credentials.RequestID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	approvedInvitation, approved, err := PollJoinApproval(ctx, network, credentials)
	if err != nil || !approved || approvedInvitation.InvitationID != invitation.InvitationID {
		t.Fatalf("approved request = %#v, %v, %v", approvedInvitation, approved, err)
	}
	_, wrongMembership, err := newMembershipDraft(filepath.Join(home, "wrong-state"), filesystemID, "fedcba9876543210", "wrong", 7845)
	if err != nil {
		t.Fatal(err)
	}
	var start PairStartResponse
	err = managed.PairCall(ctx, network.Offers[0].Endpoint, invitation.CertificateSHA256, "pair-start", PairStartRequest{
		InvitationID: invitation.InvitationID, Secret: invitation.Secret, PeerID: "fedcba9876543210", PeerName: "wrong", Membership: wrongMembership,
	}, &start)
	if err == nil {
		t.Fatalf("approval was not bound to requested peer: %v", err)
	}
	var ignored JoinStatusResponse
	if err := managed.PairCall(ctx, network.Offers[0].Endpoint, strings.Repeat("0", 64), "join-status", JoinStatusRequest{
		RequestID: credentials.RequestID, Secret: credentials.Secret,
	}, &ignored); err == nil {
		t.Fatal("join status accepted the wrong certificate pin")
	}
}

func TestApprovalOffersSelectsOnlyPeersThatAcceptedRequest(t *testing.T) {
	offers := []Offer{{PeerName: "offline"}, {PeerName: "ares"}, {PeerName: "iris"}}
	selected := approvalOffers(offers, []string{"ares"})
	if len(selected) != 1 || selected[0].PeerName != "ares" {
		t.Fatalf("approval offers = %#v, want only ares", selected)
	}
	if fallback := approvalOffers(offers, nil); !reflect.DeepEqual(fallback, offers) {
		t.Fatalf("legacy approval offers = %#v, want all offers", fallback)
	}
}

func TestEvaluateMeshChecksEveryDirection(t *testing.T) {
	peers := map[string]MeshPeer{
		"aaaaaaaaaaaaaaaa": {PeerID: "aaaaaaaaaaaaaaaa", PeerName: "desktop"},
		"bbbbbbbbbbbbbbbb": {PeerID: "bbbbbbbbbbbbbbbb", PeerName: "laptop"},
		"cccccccccccccccc": {PeerID: "cccccccccccccccc", PeerName: "server"},
	}
	reports := map[string]DiagnosticReport{
		"aaaaaaaaaaaaaaaa": {
			PeerID: "aaaaaaaaaaaaaaaa", Remotes: []RemoteDiagnostic{
				{Name: "dfs-peer-bbbbbbbbbbbb", Reachable: true},
				{Name: "dfs-peer-cccccccccccc", Reachable: false, Error: "connection refused"},
			},
		},
		"bbbbbbbbbbbbbbbb": {
			PeerID: "bbbbbbbbbbbbbbbb", Remotes: []RemoteDiagnostic{
				{Name: "dfs-peer-aaaaaaaaaaaa", Reachable: true},
			},
		},
	}
	report := evaluateMesh(peers, reports, map[string]string{"cccccccccccccccc": "diagnostic unavailable"})
	if report.Complete || len(report.Connections) != 6 {
		t.Fatalf("mesh report = %#v", report)
	}
	want := map[string]string{
		"desktop>laptop": "OK", "desktop>server": "FAILED",
		"laptop>desktop": "OK", "laptop>server": "NOT_CONFIGURED",
		"server>desktop": "UNREPORTED", "server>laptop": "UNREPORTED",
	}
	for _, connection := range report.Connections {
		from, to := "", ""
		for _, participant := range report.Peers {
			if participant.PeerID == connection.FromPeerID {
				from = participant.PeerName
			}
			if participant.PeerID == connection.ToPeerID {
				to = participant.PeerName
			}
		}
		key := from + ">" + to
		if connection.Status != want[key] {
			t.Errorf("%s status = %q, want %q", key, connection.Status, want[key])
		}
	}
}

func TestEvaluateSetupAcknowledgementsAllowsOfflinePendingMembers(t *testing.T) {
	report := MeshReport{
		Peers: []MeshPeer{{PeerID: "a", PeerName: "ares"}, {PeerID: "z", PeerName: "zeus"}, {PeerID: "i", PeerName: "iris"}},
		Reports: []DiagnosticReport{
			{PeerID: "a", PeerName: "ares", ReconciliationStatus: "ready"},
			{PeerID: "z", PeerName: "zeus", ReconciliationStatus: "ready"},
		},
		Connections: []MeshConnection{
			{FromPeerID: "a", ToPeerID: "z", Status: "OK"}, {FromPeerID: "z", ToPeerID: "a", Status: "OK"},
			{FromPeerID: "a", ToPeerID: "i", Status: "FAILED"}, {FromPeerID: "i", ToPeerID: "a", Status: "UNREPORTED"},
		},
	}
	acknowledgements, ready := EvaluateSetupAcknowledgements(report)
	if !ready || len(acknowledgements) != 3 {
		t.Fatalf("setup acknowledgements = %#v, ready=%v", acknowledgements, ready)
	}
	statuses := make(map[string]string)
	for _, acknowledgement := range acknowledgements {
		statuses[acknowledgement.PeerName] = acknowledgement.Status
	}
	if statuses["ares"] != "READY" || statuses["zeus"] != "READY" || statuses["iris"] != "PENDING" {
		t.Fatalf("setup statuses = %#v", statuses)
	}
}

func TestCurrentMachineMembershipsSupersedePurgedPeerIdentities(t *testing.T) {
	base := time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC)
	records := []membership.Record{
		{Payload: membership.Payload{PeerID: "old-zeus", Name: "zeus", Hostname: "zeus", UpdatedAt: base}},
		{Payload: membership.Payload{PeerID: "new-zeus", Name: "zeus", Hostname: "ZEUS.", UpdatedAt: base.Add(time.Minute)}},
		{Payload: membership.Payload{PeerID: "iris", Name: "iris", Hostname: "iris", UpdatedAt: base}},
	}
	current, superseded := currentMachineMemberships(records)
	var peerIDs []string
	for _, record := range current {
		peerIDs = append(peerIDs, record.Payload.PeerID)
	}
	if !slices.Equal(peerIDs, []string{"iris", "new-zeus"}) || !superseded["old-zeus"] || superseded["new-zeus"] {
		t.Fatalf("current peers = %#v, superseded = %#v", peerIDs, superseded)
	}
	remotes := []repository.Remote{{Name: "dfs-peer-old-zeus"}, {Name: "dfs-peer-new-zeus"}, {Name: "storage"}}
	if names := supersededRemoteNames(remotes, superseded); !slices.Equal(names, []string{"dfs-peer-old-zeus"}) {
		t.Fatalf("superseded remotes = %#v", names)
	}
}

func TestCurrentMachineMembershipsKeepSameNameOnDifferentHosts(t *testing.T) {
	records := []membership.Record{
		{Payload: membership.Payload{PeerID: "first", Name: "worker", Hostname: "first-host"}},
		{Payload: membership.Payload{PeerID: "second", Name: "worker", Hostname: "second-host"}},
	}
	current, superseded := currentMachineMemberships(records)
	if len(current) != 2 || len(superseded) != 0 {
		t.Fatalf("current peers = %#v, superseded = %#v", current, superseded)
	}
}

func TestEvaluateSetupAcknowledgementsRejectsIncompleteOnlineDirection(t *testing.T) {
	report := MeshReport{
		Peers: []MeshPeer{{PeerID: "a", PeerName: "ares"}, {PeerID: "z", PeerName: "zeus"}},
		Reports: []DiagnosticReport{
			{PeerID: "a", PeerName: "ares", ReconciliationStatus: "ready"},
			{PeerID: "z", PeerName: "zeus", ReconciliationStatus: "ready"},
		},
		Connections: []MeshConnection{{FromPeerID: "a", ToPeerID: "z", Status: "OK"}, {FromPeerID: "z", ToPeerID: "a", Status: "NOT_CONFIGURED"}},
	}
	acknowledgements, ready := EvaluateSetupAcknowledgements(report)
	if ready {
		t.Fatalf("incomplete directed cluster accepted: %#v", acknowledgements)
	}
	if acknowledgements[1].Status != "INCOMPLETE" || !strings.Contains(acknowledgements[1].Detail, "ares") {
		t.Fatalf("incomplete acknowledgement = %#v", acknowledgements[1])
	}
}

func TestEvaluateSetupAcknowledgementsRequiresDiscoveredPeerResponse(t *testing.T) {
	report := MeshReport{
		Peers:   []MeshPeer{{PeerID: "a", PeerName: "ares", Online: true}, {PeerID: "z", PeerName: "zeus", Online: true}},
		Reports: []DiagnosticReport{{PeerID: "a", PeerName: "ares", ReconciliationStatus: "ready"}},
		Connections: []MeshConnection{
			{FromPeerID: "a", ToPeerID: "z", Status: "FAILED"}, {FromPeerID: "z", ToPeerID: "a", Status: "UNREPORTED"},
		},
	}
	acknowledgements, ready := EvaluateSetupAcknowledgements(report)
	if ready || acknowledgements[1].Status != "INCOMPLETE" {
		t.Fatalf("discovered peer acknowledgement = %#v, ready=%v", acknowledgements, ready)
	}
}

func TestEvaluateMeshDetectsNamespaceDivergence(t *testing.T) {
	peers := map[string]MeshPeer{
		"aaaaaaaaaaaaaaaa": {PeerID: "aaaaaaaaaaaaaaaa", PeerName: "desktop"},
		"bbbbbbbbbbbbbbbb": {PeerID: "bbbbbbbbbbbbbbbb", PeerName: "laptop"},
	}
	reports := map[string]DiagnosticReport{
		"aaaaaaaaaaaaaaaa": {PeerID: "aaaaaaaaaaaaaaaa", PeerName: "desktop", TreeID: "tree-a", Remotes: []RemoteDiagnostic{{Name: "dfs-peer-bbbbbbbbbbbb", Reachable: true, Transport: "quic"}}},
		"bbbbbbbbbbbbbbbb": {PeerID: "bbbbbbbbbbbbbbbb", PeerName: "laptop", TreeID: "tree-b", Remotes: []RemoteDiagnostic{{Name: "dfs-peer-aaaaaaaaaaaa", Reachable: true, Transport: "quic"}}},
	}
	report := evaluateMesh(peers, reports, nil)
	if report.Complete || report.NamespaceStatus != "inconsistent" || len(report.Issues) != 1 || report.Issues[0].Code != "NAMESPACE_DIVERGED" {
		t.Fatalf("mesh report = %+v", report)
	}
}

func TestEvaluateMeshDetectsClusterPinPolicyDivergence(t *testing.T) {
	peers := map[string]MeshPeer{
		"aaaaaaaaaaaaaaaa": {PeerID: "aaaaaaaaaaaaaaaa", PeerName: "desktop"},
		"bbbbbbbbbbbbbbbb": {PeerID: "bbbbbbbbbbbbbbbb", PeerName: "laptop"},
	}
	reports := map[string]DiagnosticReport{
		"aaaaaaaaaaaaaaaa": {PeerID: "aaaaaaaaaaaaaaaa", PeerName: "desktop", TreeID: "same-tree",
			Stats:   repository.HealthStats{Pinned: []repository.PinnedPathHealth{{Path: "Media", Scope: "cluster", Status: "ready"}}},
			Remotes: []RemoteDiagnostic{{Name: "dfs-peer-bbbbbbbbbbbb", Reachable: true, Transport: "quic"}}},
		"bbbbbbbbbbbbbbbb": {PeerID: "bbbbbbbbbbbbbbbb", PeerName: "laptop", TreeID: "same-tree",
			Remotes: []RemoteDiagnostic{{Name: "dfs-peer-aaaaaaaaaaaa", Reachable: true, Transport: "quic"}}},
	}
	report := evaluateMesh(peers, reports, nil)
	if report.Complete || len(report.Issues) != 1 || report.Issues[0].Code != "CLUSTER_PIN_POLICY_DIVERGED" {
		t.Fatalf("cluster pin health report = %+v", report)
	}
}

func TestMeshPeerIDForRemoteUsesConfiguredShortID(t *testing.T) {
	peers := map[string]MeshPeer{
		"6a728fd84bbb1234567890abcdefabcd": {PeerID: "6a728fd84bbb1234567890abcdefabcd", PeerName: "zeus"},
	}
	if got := meshPeerIDForRemote(peers, "dfs-peer-6a728fd84bbb"); got != "6a728fd84bbb1234567890abcdefabcd" {
		t.Fatalf("peer ID = %q", got)
	}
	if got := meshPeerIDForRemote(peers, "dfs-peer-fa0841841386"); got != "" {
		t.Fatalf("unknown peer ID = %q", got)
	}
}

func TestPairingCertificatePersists(t *testing.T) {
	home := t.TempDir()
	certificate, fingerprint, err := loadOrCreateCertificate(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(certificate.Certificate) == 0 || len(fingerprint) != 64 {
		t.Fatalf("generated certificate fingerprint = %q", fingerprint)
	}
	// Pairing tests exercise certificate pin acceptance. Guard the persisted
	// certificate from changing on reload.
	_, reloaded, err := loadOrCreateCertificate(home)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded != fingerprint {
		t.Fatalf("certificate fingerprint changed: %q != %q", reloaded, fingerprint)
	}
}

func TestCompletePairingResumesAndRemovesState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git-annex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Resume Test\nemail = resume@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo, err := repository.Init(ctx, filepath.Join(home, "source"), "source", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service, err := startService(repo, nil, listener, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var reconciliationScheduled atomic.Bool
	service.runBackground = func(func()) { reconciliationScheduled.Store(true) }
	invitation, err := CreateInvitation(repo, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	invitation.QUICEndpoint = "quic://" + listener.Addr().String()
	peerID := "123456789abcdef0"
	_, draft, err := newMembershipDraft(filepath.Join(home, "draft"), invitation.FileSystemID, peerID, "resuming", 7844)
	if err != nil {
		t.Fatal(err)
	}
	var start PairStartResponse
	if err := managed.PairCall(ctx, invitation.QUICEndpoint, invitation.CertificateSHA256, "pair-start", PairStartRequest{
		InvitationID: invitation.InvitationID, Secret: invitation.Secret, PeerID: peerID, PeerName: "resuming", Membership: draft,
	}, &start); err != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(home, "resume")
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git", "dfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := savePairingResume(repositoryPath, pairingResume{
		Version: ProtocolVersion, Endpoint: invitation.QUICEndpoint, CertificateSHA256: invitation.CertificateSHA256,
		SessionID: start.SessionID, CompletionSecret: start.CompletionSecret,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := CompletePairing(context.Background(), repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemoteName != "dfs-peer-123456789abc" {
		t.Fatalf("completion result = %#v", result)
	}
	if !reconciliationScheduled.Load() {
		t.Fatal("pair completion did not schedule membership reconciliation")
	}
	if _, err := os.Stat(filepath.Join(repositoryPath, ".git", "dfs", pairingResumeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pairing resume state remains: %v", err)
	}
}

func TestInvitationRecordsNeverStoreBearerSecret(t *testing.T) {
	repositoryPath := t.TempDir()
	record := invitationRecord{
		Version: ProtocolVersion, ID: "test", SecretHash: secretHash("bearer-secret"),
		FileSystemID: strings.Repeat("a", 40), ExpiresAt: time.Now().Add(time.Minute),
		Pending: &pendingPair{SessionID: "session", CompletionHash: secretHash("completion-secret")},
	}
	if err := saveInvitation(repositoryPath, record); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(invitationPath(repositoryPath, record.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"bearer-secret", "completion-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("invitation record contains plaintext bearer secret %q", secret)
		}
	}
	var decoded invitationRecord
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.SecretHash != record.SecretHash {
		t.Fatalf("saved invitation = %#v, %v", decoded, err)
	}
}

func TestRevokeInvitationRemovesPendingSSHAuthorization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDirectory := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "dfs-peer-123456789abc-abcdef123456"
	authorizedPath := filepath.Join(sshDirectory, "authorized_keys")
	if err := os.WriteFile(authorizedPath, []byte("ssh-ed25519 unrelated keep\nssh-ed25519 pending "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(home, "repository")
	record := invitationRecord{
		Version: ProtocolVersion, ID: "pending", SecretHash: secretHash("secret"),
		FileSystemID: strings.Repeat("a", 40), ExpiresAt: time.Now().Add(time.Minute),
		Pending: &pendingPair{AuthorizedMarker: marker},
	}
	if err := saveInvitation(repositoryPath, record); err != nil {
		t.Fatal(err)
	}
	if err := RevokeInvitation(repositoryPath, record.ID); err != nil {
		t.Fatal(err)
	}
	authorized, err := os.ReadFile(authorizedPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(authorized), marker) || !strings.Contains(string(authorized), "unrelated") {
		t.Fatalf("authorized keys after revocation:\n%s", authorized)
	}
	if _, err := os.Stat(invitationPath(repositoryPath, record.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked invitation remains: %v", err)
	}
}

func TestListInvitationsReleasesExpiredPairingLease(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sshDirectory := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := "dfs-peer-expired"
	authorizedPath := filepath.Join(sshDirectory, "authorized_keys")
	if err := os.WriteFile(authorizedPath, []byte("ssh-ed25519 pending "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(home, "repository")
	record := invitationRecord{Version: ProtocolVersion, ID: "leased", SecretHash: secretHash("secret"),
		FileSystemID: strings.Repeat("a", 40), ExpiresAt: time.Now().Add(time.Hour),
		Pending: &pendingPair{AuthorizedMarker: marker, ExpiresAt: time.Now().Add(-time.Second)}}
	if err := saveInvitation(repositoryPath, record); err != nil {
		t.Fatal(err)
	}
	infos, err := ListInvitations(repositoryPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Pending {
		t.Fatalf("released invitation = %#v", infos)
	}
	saved, err := loadInvitation(repositoryPath, record.ID)
	if err != nil || saved.Pending != nil {
		t.Fatalf("saved invitation pending = %#v, %v", saved.Pending, err)
	}
	authorized, err := os.ReadFile(authorizedPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(authorized), marker) {
		t.Fatalf("expired authorization remains: %s", authorized)
	}
}

func TestPairingAttemptRateLimit(t *testing.T) {
	service := &Service{attempts: make(map[string]attemptWindow)}
	now := time.Now()
	for attempt := 0; attempt < 10; attempt++ {
		if !service.allowPairingAttempt("192.0.2.1", now) {
			t.Fatalf("attempt %d rejected too early", attempt+1)
		}
		service.recordPairingFailure("192.0.2.1", now)
	}
	if service.allowPairingAttempt("192.0.2.1", now) {
		t.Fatal("eleventh pairing attempt was not rate-limited")
	}
	if !service.allowPairingAttempt("192.0.2.1", now.Add(time.Minute)) {
		t.Fatal("pairing attempt window did not expire")
	}
}

func gitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
