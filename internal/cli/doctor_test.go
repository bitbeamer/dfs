package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bitbeamer/dfs/internal/peer"
)

func TestPrintMeshReport(t *testing.T) {
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
		"FROM\tTO\tSTATUS\tDETAIL\n",
		"desktop (a)\tlaptop (b)\tOK\t\n",
		"laptop (b)\tdesktop (a)\tFAILED\tconnection refused\n",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("mesh output does not contain %q:\n%s", expected, output.String())
		}
	}
}
