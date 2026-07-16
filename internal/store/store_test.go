package store

import (
	"path/filepath"
	"testing"
)

func TestPinsApplyToDescendants(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.Pin("Photos/Vacation"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"Photos/Vacation", "Photos/Vacation/a.jpg"} {
		pinned, err := state.IsPinned(path)
		if err != nil || !pinned {
			t.Fatalf("IsPinned(%q) = %v, %v", path, pinned, err)
		}
	}
	pinned, err := state.IsPinned("Photos/Other/a.jpg")
	if err != nil || pinned {
		t.Fatalf("unrelated path pinned=%v, err=%v", pinned, err)
	}
	if err := state.Unpin("Photos/Vacation"); err != nil {
		t.Fatal(err)
	}
	pinned, err = state.IsPinned("Photos/Vacation/a.jpg")
	if err != nil || pinned {
		t.Fatalf("path remains pinned=%v, err=%v", pinned, err)
	}
}

func TestTouchRecordsAccess(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.Touch("file.txt"); err != nil {
		t.Fatal(err)
	}
	if state.LastAccess("file.txt").IsZero() {
		t.Fatal("access time was not recorded")
	}
}
