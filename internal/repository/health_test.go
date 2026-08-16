package repository

import (
	"reflect"
	"testing"

	"github.com/bitbeamer/dfs/internal/store"
)

func TestParseAnnexHealthFiles(t *testing.T) {
	files, err := parseAnnexHealthFiles("{\"file\":\"Archive/item.txt\",\"bytesize\":\"42\"}\n")
	if err != nil || len(files) != 1 || files[0].Path != "Archive/item.txt" || files[0].Size != 42 {
		t.Fatalf("files = %#v, %v", files, err)
	}
}

func TestPathMatchesAnyPin(t *testing.T) {
	for _, test := range []struct {
		path string
		pins []string
		want bool
	}{
		{"Archive/item.txt", []string{"Archive"}, true},
		{"Archive/item.txt", []string{"Archive/item.txt"}, true},
		{"Other/item.txt", []string{"Archive"}, false},
	} {
		if got := pathMatchesAnyPin(test.path, test.pins); got != test.want {
			t.Errorf("pathMatchesAnyPin(%q, %q) = %v", test.path, test.pins, got)
		}
	}
}

func TestPinnedPathHealthReportsFilesAndDirectoryTotals(t *testing.T) {
	files := map[string]int64{"photo.jpg": 12, "albums/one.jpg": 20, "albums/two.jpg": 30}
	annexed := map[string]annexHealthFile{
		"photo.jpg":      {Path: "photo.jpg", Size: 12},
		"albums/one.jpg": {Path: "albums/one.jpg", Size: 20},
		"albums/two.jpg": {Path: "albums/two.jpg", Size: 30},
	}
	local := map[string]annexHealthFile{"photo.jpg": annexed["photo.jpg"], "albums/one.jpg": annexed["albums/one.jpg"]}
	got := pinnedPathHealth([]store.Pin{{Path: "albums", Scope: "cluster"}, {Path: "missing", Scope: "local"}, {Path: "photo.jpg", Scope: "local"}}, files, local, annexed)
	want := []PinnedPathHealth{
		{Path: "albums", Scope: "cluster", Kind: "directory", LogicalFiles: 2, LogicalBytes: 50, MissingFiles: 1, MissingBytes: 30},
		{Path: "missing", Scope: "local", Kind: "missing"},
		{Path: "photo.jpg", Scope: "local", Kind: "file", LogicalFiles: 1, LogicalBytes: 12},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned path health = %#v, want %#v", got, want)
	}
}

func TestPinHealthDistinguishesReadyHydratingAndCapacity(t *testing.T) {
	pins := []PinnedPathHealth{
		{Kind: "file"},
		{Kind: "file", MissingFiles: 1, MissingBytes: 10},
		{Kind: "file", MissingFiles: 1, MissingBytes: 200},
	}
	updatePinStatuses(pins, 100)
	got := []string{pins[0].Status, pins[1].Status, pins[2].Status}
	want := []string{"ready", "hydrating", "capacity-constrained"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pin statuses = %v, want %v", got, want)
	}
}
