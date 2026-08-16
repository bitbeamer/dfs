package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/bitbeamer/dfs/internal/peer"
)

func TestPrintClusterConnectionReport(t *testing.T) {
	report := peer.MeshReport{
		Peers: []peer.MeshPeer{
			{PeerID: "a", PeerName: "desktop"},
			{PeerID: "b", PeerName: "laptop"},
		},
		Connections: []peer.MeshConnection{
			{FromPeerID: "a", ToPeerID: "b", Status: "OK"},
			{FromPeerID: "b", ToPeerID: "a", Status: "FAILED", Error: "connection refused"},
		},
	}
	var output bytes.Buffer
	printMeshReport(&output, report)
	for _, expected := range []string{
		"FROM     TO       STATUS  DETAIL",
		"desktop  laptop   OK      QUIC",
		"laptop   desktop  FAILED  connection refused",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("cluster output does not contain %q:\n%s", expected, output.String())
		}
	}
}

func TestPrepareDoctorPathAddsHomebrewForNoninteractiveMacOS(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	if err := prepareDoctorPath("darwin"); err != nil {
		t.Fatal(err)
	}
	if got, want := os.Getenv("PATH"), "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if err := prepareDoctorPath("darwin"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(os.Getenv("PATH"), "/opt/homebrew/bin") != 1 {
		t.Fatalf("PATH contains duplicate Homebrew entries: %q", os.Getenv("PATH"))
	}
}
