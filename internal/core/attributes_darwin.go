//go:build darwin

package core

import (
	"os"
	"syscall"
	"time"
)

func applyPlatformAttributes(info os.FileInfo, attributes *Attributes) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	attributes.UID = stat.Uid
	attributes.GID = stat.Gid
	attributes.Inode = stat.Ino
	attributes.Blocks = stat.Blocks
	attributes.Accessed = time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec)
	attributes.Changed = time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec)
}
