//go:build linux

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
	attributes.Accessed = time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
	attributes.Changed = time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
}
