package store

import (
	"errors"
	"path/filepath"
	"reflect"
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

func TestFileMetadataAndXAttrsFollowNamespaceChanges(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	metadata := FileMetadata{Mode: 0o100750, UID: 1, GID: 2, AtimeNS: 3, MtimeNS: 4, CtimeNS: 5, Signature: "abc"}
	if err := state.SaveFileMetadata("old.txt", metadata); err != nil {
		t.Fatal(err)
	}
	if err := state.SetXAttr("old.txt", "user.one", []byte("value"), 0); err != nil {
		t.Fatal(err)
	}
	if err := state.SetXAttr("old.txt/child", "user.child", []byte("child"), 0); err != nil {
		t.Fatal(err)
	}
	if err := state.SetXAttr("old.txt", "user.one", []byte("again"), 1); !errors.Is(err, ErrXAttrExists) {
		t.Fatalf("create existing xattr = %v", err)
	}
	if err := state.SetXAttr("old.txt", "user.missing", []byte("value"), 2); !errors.Is(err, ErrXAttrNotFound) {
		t.Fatalf("replace missing xattr = %v", err)
	}
	if err := state.RenameFileState("old.txt", "new.txt"); err != nil {
		t.Fatal(err)
	}
	gotMetadata, found, err := state.FileMetadata("new.txt")
	if err != nil || !found || gotMetadata != metadata {
		t.Fatalf("renamed metadata = %+v, %v, %v", gotMetadata, found, err)
	}
	value, err := state.XAttr("new.txt", "user.one")
	if err != nil || string(value) != "value" {
		t.Fatalf("renamed xattr = %q, %v", value, err)
	}
	if value, err := state.XAttr("new.txt/child", "user.child"); err != nil || string(value) != "child" {
		t.Fatalf("renamed descendant xattr = %q, %v", value, err)
	}
	if names, err := state.ListXAttrs("new.txt"); err != nil || !reflect.DeepEqual(names, []string{"user.one"}) {
		t.Fatalf("xattr names = %q, %v", names, err)
	}
	if err := state.RemoveFileState("new.txt"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := state.FileMetadata("new.txt"); err != nil || found {
		t.Fatalf("removed metadata found=%v err=%v", found, err)
	}
	if _, err := state.XAttr("new.txt", "user.one"); !errors.Is(err, ErrXAttrNotFound) {
		t.Fatalf("removed xattr = %v", err)
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
