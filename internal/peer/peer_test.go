package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/hashicorp/mdns"
)

func TestInvitationRoundTripAndValidation(t *testing.T) {
	invitation := Invitation{
		Version: ProtocolVersion, FileSystemID: strings.Repeat("a", 40), InvitationID: "invite",
		Secret: "secret", CertificateSHA256: strings.Repeat("b", 64), Endpoint: "https://desktop.local:1234",
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

func TestOffersFromEntry(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Host: "desktop.local.", AddrV4: net.ParseIP("192.0.2.10"), Port: 44123,
		InfoFields: []string{
			"v=1", "fs=" + strings.Repeat("a", 40), "network=" + encodeTXT("Home Files"),
			"peer=0123456789abcdef", "name=" + encodeTXT("Desktop"), "cert=" + strings.Repeat("b", 64),
		},
	}
	offers := offersFromEntry(entry)
	if len(offers) != 1 {
		t.Fatalf("offers = %#v", offers)
	}
	offer := offers[0]
	if offer.NetworkName != "Home Files" || offer.PeerName != "Desktop" || offer.Endpoint != "https://192.0.2.10:44123" {
		t.Fatalf("offer = %#v", offer)
	}

	entry.InfoFields = []string{"v=broken"}
	if offers := offersFromEntry(entry); len(offers) != 0 {
		t.Fatalf("invalid entry produced offers: %#v", offers)
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
	for _, command := range []string{"git-annex", "ssh", "rsync"} {
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
	loopback, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	previousProvider := interfaceProvider
	interfaceProvider = func() []*net.Interface { return []*net.Interface{loopback} }
	defer func() { interfaceProvider = previousProvider }()
	service, err := Start(repo, nil, -1)
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
	for _, command := range []string{"git-annex", "ssh", "rsync"} {
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
	service, err := startService(existing, nil, listener, false)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	invitation, err := CreateInvitation(existing, 5*time.Minute, existing.Config.Repository)
	if err != nil {
		t.Fatal(err)
	}
	invitation.Endpoint = "https://" + listener.Addr().String()
	encoded, err := invitation.Encode()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(home, "laptop")
	result, err := PairAndJoin(ctx, encoded, destination, "laptop", 5<<20, 20*time.Millisecond, true)
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
	remotes, err := existing.Remotes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var foundReverse bool
	for _, remote := range remotes {
		if remote.Name == result.ReverseRemoteName && strings.Contains(remote.URL, destination) {
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
	authorized, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"restrict,command=", " peer serve", existing.Config.Repository, destination, "dfs-peer-"} {
		if !strings.Contains(string(authorized), expected) {
			t.Fatalf("authorized_keys does not contain %q:\n%s", expected, authorized)
		}
	}
	for _, repo := range []*repository.Repository{existing, result.Repository} {
		output := gitOutput(t, repo.Config.Repository, "config", "--get", "core.sshCommand")
		if !strings.Contains(output, transportKeyFile) || !strings.Contains(output, "BatchMode=yes") {
			t.Fatalf("paired SSH command = %q", output)
		}
		info, err := os.Stat(filepath.Join(repo.Config.Repository, ".git", "dfs", transportKeyFile))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("transport key mode = %v, %v", info, err)
		}
	}
	active, err := ListInvitations(existing.Config.Repository, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("completed invitation remains active: %#v", active)
	}
}

func TestServeSSHDelegatesToRepositoryRestrictedAnnexShell(t *testing.T) {
	directory := t.TempDir()
	capture := filepath.Join(directory, "capture")
	script := filepath.Join(directory, "git-annex-shell")
	contents := "#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$1\" \"$2\" \"$GIT_ANNEX_SHELL_DIRECTORY\" > \"$CAPTURE_PATH\"\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("SSH_ORIGINAL_COMMAND", "git-upload-pack '/some/repository'")
	repositoryPath := filepath.Join(directory, "repository")
	if err := ServeSSH(repositoryPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "-c\ngit-upload-pack '/some/repository'\n"+repositoryPath+"\n" {
		t.Fatalf("git-annex-shell invocation:\n%s", data)
	}
}

func TestPairingRejectsWrongCertificatePin(t *testing.T) {
	home := t.TempDir()
	certificate, fingerprint, err := loadOrCreateCertificate(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(certificate.Certificate) == 0 || len(fingerprint) != 64 {
		t.Fatalf("generated certificate fingerprint = %q", fingerprint)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer server.Close()
	client := pinnedHTTPClient(strings.Repeat("0", 64))
	defer client.CloseIdleConnections()
	if _, err := client.Get(server.URL); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong certificate pin error = %v", err)
	}
	// The full pairing test exercises the accepted pin. Also guard the
	// persisted certificate from changing on reload.
	_, reloaded, err := loadOrCreateCertificate(home)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded != fingerprint {
		t.Fatalf("certificate fingerprint changed: %q != %q", reloaded, fingerprint)
	}
}

func TestCompletePairingResumesAndRemovesState(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var input PairCompleteRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.SessionID != "session" || input.CompletionSecret != "completion" {
			http.Error(response, "bad completion", http.StatusBadRequest)
			return
		}
		writeJSON(response, http.StatusOK, PairCompleteResponse{RemoteName: "dfs-peer-123456789abc"})
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer server.Close()
	digest := sha256.Sum256(server.Certificate().Raw)
	repositoryPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repositoryPath, ".git", "dfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := savePairingResume(repositoryPath, pairingResume{
		Version: ProtocolVersion, Endpoint: server.URL, CertificateSHA256: hex.EncodeToString(digest[:]),
		SessionID: "session", CompletionSecret: "completion",
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
