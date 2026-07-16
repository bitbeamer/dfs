package mount

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/syncer"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
)

func Run(repo *repository.Repository, mountpoint string, foreground bool) error {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("create mountpoint: %w", err)
	}
	scheduler := syncer.New(repo, repo.Config.SyncInterval, os.Stderr)
	scheduler.Start()
	defer scheduler.Stop()

	filesystem := NewFileSystem(repo, scheduler)
	pathNodes := pathfs.NewPathNodeFs(filesystem, &pathfs.PathNodeFsOptions{ClientInodes: true})
	mountOptions := &fuse.MountOptions{
		FsName: "dfs", Name: "dfs", DisableXAttrs: false,
		Options: []string{"default_permissions"},
	}
	options := nodefs.NewOptions()
	options.EntryTimeout = time.Second
	options.AttrTimeout = time.Second
	server, _, err := nodefs.Mount(mountpoint, pathNodes.Root(), mountOptions, options)
	if err != nil {
		return fmt.Errorf("mount DFS at %s: %w", mountpoint, err)
	}
	server.Serve()
	return nil
}

func Unmount(mountpoint string) error {
	var command string
	var args []string
	if runtime.GOOS == "darwin" {
		command, args = "umount", []string{mountpoint}
	} else {
		command, args = "fusermount3", []string{"-u", mountpoint}
	}
	output, err := exec.Command(command, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unmount %s: %s", mountpoint, string(output))
	}
	return nil
}
