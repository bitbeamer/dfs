package mount

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

type countingNotifier struct {
	writers int
}

func (n *countingNotifier) Notify(string) {}
func (n *countingNotifier) BeginWrite()   { n.writers++ }
func (n *countingNotifier) EndWrite()     { n.writers-- }

type failingDurabilityFile struct {
	nodefs.File
	flushCode fuse.Status
	fsyncCode fuse.Status
}

func (f *failingDurabilityFile) Flush() fuse.Status    { return f.flushCode }
func (f *failingDurabilityFile) Fsync(int) fuse.Status { return f.fsyncCode }

func TestStagedFilePropagatesFlushAndFsyncErrors(t *testing.T) {
	noSpace := fuse.Status(syscall.ENOSPC)
	underlying := &failingDurabilityFile{
		File: nodefs.NewDefaultFile(), flushCode: fuse.EIO, fsyncCode: noSpace,
	}
	transaction := &writeTransaction{}
	file := &stagedFile{File: underlying, filesystem: &FileSystem{}, transaction: transaction}
	if code := file.Flush(); code != fuse.EIO {
		t.Fatalf("Flush = %v, want EIO", code)
	}
	if transaction.failure != fuse.EIO {
		t.Fatalf("transaction failure = %v, want EIO", transaction.failure)
	}
	transaction.failure = fuse.OK
	if code := file.Fsync(0); code != noSpace {
		t.Fatalf("Fsync = %v, want ENOSPC", code)
	}
	if transaction.failure != noSpace {
		t.Fatalf("transaction failure = %v, want ENOSPC", transaction.failure)
	}
}

func TestFlushDefersPublicationAndSyncUntilRelease(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	notifier := &countingNotifier{}
	filesystem.notifier = notifier
	opened, err := filesystem.openStaged("note.txt", syscall.O_WRONLY|syscall.O_TRUNC, 0o644, true)
	if err != nil {
		t.Fatal(err)
	}
	file := opened.(*stagedFile)
	if _, code := file.Write([]byte("first"), 0); code != fuse.OK {
		t.Fatalf("first write = %v", code)
	}
	if notifier.writers != 1 {
		t.Fatalf("open writers before flush = %d, want 1", notifier.writers)
	}
	if code := file.Flush(); code != fuse.OK {
		t.Fatalf("flush = %v", code)
	}
	if notifier.writers != 1 {
		t.Fatalf("open writers after flush = %d, want 1", notifier.writers)
	}
	if content, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || len(content) != 0 {
		t.Fatalf("flush published staged content = %q, %v", content, err)
	}
	if _, code := file.Write([]byte(" second"), 5); code != fuse.OK {
		t.Fatalf("second write = %v", code)
	}
	if notifier.writers != 1 {
		t.Fatalf("open writers after continued write = %d, want 1", notifier.writers)
	}
	file.Release()
	if notifier.writers != 0 {
		t.Fatalf("open writers after release = %d, want 0", notifier.writers)
	}
	if content, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(content) != "first second" {
		t.Fatalf("published content = %q, %v", content, err)
	}
}
