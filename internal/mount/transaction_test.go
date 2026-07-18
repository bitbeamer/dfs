package mount

import (
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

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
