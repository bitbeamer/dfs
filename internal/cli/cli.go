package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	dfsmount "github.com/bitbeamer/dfs/internal/mount"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/spf13/cobra"
)

var Version = "dev"

type App struct {
	Out  io.Writer
	Err  io.Writer
	repo string
}

func New() *cobra.Command {
	app := &App{Out: os.Stdout, Err: os.Stderr}
	root := &cobra.Command{
		Use:           "dfs",
		Short:         "A quota-aware distributed filesystem built on Git and git-annex",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.SetOut(app.Out)
	root.SetErr(app.Err)
	root.PersistentFlags().StringVar(&app.repo, "repo", "", "DFS repository (or set DFS_REPO)")
	root.AddCommand(
		app.initCommand(), app.joinCommand(), app.peerCommand(), app.relayCommand(),
		app.storageCommand(),
		app.mountCommand(), app.unmountCommand(), app.syncCommand(), app.statusCommand(),
		app.fetchCommand(), app.pinCommand(), app.unpinCommand(), app.evictCommand(),
		app.cacheCommand(), app.historyCommand(), app.restoreCommand(), app.conflictsCommand(),
		app.doctorCommand(),
	)
	return root
}

func Execute() error { return New().Execute() }

func (a *App) open() (*repository.Repository, error) { return repository.Open(a.repo) }

func commandContext(command *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(command.Context(), 24*time.Hour)
}

func (a *App) initCommand() *cobra.Command {
	var name, limit, relay string
	cmd := &cobra.Command{
		Use:   "init <repository>",
		Short: "Create a new DFS repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, err := config.ParseSize(limit)
			if err != nil {
				return err
			}
			ctx, cancel := commandContext(cmd)
			defer cancel()
			repo, err := repository.Init(ctx, args[0], name, bytes)
			if err != nil {
				return err
			}
			defer repo.Close()
			if relay != "" {
				if err := repo.SetRelay(ctx, relay); err != nil {
					return err
				}
			}
			fmt.Fprintf(a.Out, "Initialized DFS repository for %s at %s\n", repo.Config.Name, repo.Config.Repository)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "peer name (defaults to hostname)")
	cmd.Flags().StringVar(&limit, "cache-limit", "100GiB", "maximum local content cache")
	cmd.Flags().StringVar(&relay, "relay", "", "optional bare Git metadata relay URL")
	return cmd
}

func (a *App) joinCommand() *cobra.Command {
	var name, limit string
	cmd := &cobra.Command{
		Use:   "join <git-url> <repository>",
		Short: "Clone and join an existing DFS repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, err := config.ParseSize(limit)
			if err != nil {
				return err
			}
			ctx, cancel := commandContext(cmd)
			defer cancel()
			repo, err := repository.Join(ctx, args[0], args[1], name, bytes)
			if err != nil {
				return err
			}
			defer repo.Close()
			fmt.Fprintf(a.Out, "Joined DFS repository as %s at %s\n", repo.Config.Name, repo.Config.Repository)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "peer name (defaults to hostname)")
	cmd.Flags().StringVar(&limit, "cache-limit", "100GiB", "maximum local content cache")
	return cmd
}

func (a *App) peerCommand() *cobra.Command {
	peer := &cobra.Command{Use: "peer", Short: "Manage direct Git/git-annex peers"}
	peer.AddCommand(
		&cobra.Command{
			Use: "add <name> <ssh-url>", Args: cobra.ExactArgs(2), Short: "Add a peer",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				return repo.AddRemote(ctx, args[0], args[1])
			},
		},
		&cobra.Command{
			Use: "remove <name>", Args: cobra.ExactArgs(1), Short: "Remove a peer",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				return repo.RemovePeer(ctx, args[0])
			},
		},
		&cobra.Command{
			Use: "list", Args: cobra.NoArgs, Short: "List peers and remotes",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				remotes, err := repo.Remotes(ctx)
				if err != nil {
					return err
				}
				for _, remote := range remotes {
					fmt.Fprintf(a.Out, "%s\t%s\n", remote.Name, remote.URL)
				}
				return nil
			},
		},
	)
	return peer
}

func (a *App) relayCommand() *cobra.Command {
	relay := &cobra.Command{Use: "relay", Short: "Manage the optional metadata relay"}
	relay.AddCommand(
		&cobra.Command{
			Use: "set <git-url>", Args: cobra.ExactArgs(1), Short: "Configure the bare Git relay",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				return repo.SetRelay(ctx, args[0])
			},
		},
		&cobra.Command{
			Use: "status", Args: cobra.NoArgs, Short: "Show relay configuration",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				if repo.Config.Relay == "" {
					fmt.Fprintln(a.Out, "No metadata relay configured")
				} else {
					fmt.Fprintln(a.Out, repo.Config.Relay)
				}
				return nil
			},
		},
	)
	return relay
}

func (a *App) storageCommand() *cobra.Command {
	storage := &cobra.Command{Use: "storage", Short: "Manage durable git-annex storage"}
	var bucket, region, host, encryption string
	addS3 := &cobra.Command{
		Use: "add-s3 <name>", Args: cobra.ExactArgs(1), Short: "Add an S3-compatible special remote",
		RunE: func(cmd *cobra.Command, args []string) error {
			if bucket == "" {
				return fmt.Errorf("--bucket is required")
			}
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			return repo.InitS3(ctx, args[0], bucket, region, host, encryption)
		},
	}
	addS3.Flags().StringVar(&bucket, "bucket", "", "S3 bucket name")
	addS3.Flags().StringVar(&region, "region", "", "S3 region")
	addS3.Flags().StringVar(&host, "host", "", "optional S3-compatible endpoint host")
	addS3.Flags().StringVar(&encryption, "encryption", "shared", "git-annex encryption mode")
	storage.AddCommand(
		addS3,
		&cobra.Command{
			Use: "enable <name>", Args: cobra.ExactArgs(1), Short: "Enable an existing special remote on this peer",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				return repo.EnableStorage(ctx, args[0])
			},
		},
		&cobra.Command{
			Use: "copy <name> <path>...", Args: cobra.MinimumNArgs(2), Short: "Copy content to durable storage",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				return repo.CopyTo(ctx, args[0], args[1:])
			},
		},
	)
	return storage
}

func (a *App) mountCommand() *cobra.Command {
	var logLevel, logFile string
	var fuseDebug, recoverStaleSession bool
	cmd := &cobra.Command{
		Use: "mount <mountpoint>", Args: cobra.ExactArgs(1), Short: "Mount the DFS namespace and run automatic sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			mountSignals := make(chan os.Signal, 2)
			signal.Notify(mountSignals, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(mountSignals)
			logger, closer, err := newMountLogger(logLevel, logFile, a.Err, fuseDebug)
			if err != nil {
				return err
			}
			if closer != nil {
				defer closer.Close()
			}
			repo, err := a.open()
			if err != nil {
				logger.Error("opening repository failed", "error", err)
				return err
			}
			defer repo.Close()
			fmt.Fprintf(a.Out, "Mounting %s at %s; press Ctrl-C to stop\n", repo.Config.Repository, args[0])
			return dfsmount.Run(repo, args[0], dfsmount.Options{
				Context: cmd.Context(), Logger: logger, FUSEDebug: fuseDebug,
				RecoverStaleSession: recoverStaleSession, Signals: mountSignals,
			})
		},
	}
	cmd.Flags().StringVar(&logLevel, "log-level", "error", "logging level: debug, info, warn, or error")
	cmd.Flags().StringVar(&logFile, "log-file", "", "append logs to this file as well as stderr")
	cmd.Flags().BoolVar(&fuseDebug, "fuse-debug", false, "log low-level FUSE protocol requests and enable debug logging (very noisy)")
	cmd.Flags().BoolVar(&recoverStaleSession, "recover-stale-session", false, "take over after verifying another host's recorded mount is inactive")
	return cmd
}

func (a *App) unmountCommand() *cobra.Command {
	return &cobra.Command{
		Use: "unmount <mountpoint>", Aliases: []string{"umount"}, Args: cobra.ExactArgs(1), Short: "Unmount a DFS namespace",
		RunE: func(cmd *cobra.Command, args []string) error { return dfsmount.Unmount(args[0]) },
	}
}

func (a *App) syncCommand() *cobra.Command {
	var metadataOnly bool
	cmd := &cobra.Command{
		Use: "sync", Args: cobra.NoArgs, Short: "Synchronize immediately",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			if err := repo.Sync(ctx, true); err != nil {
				return err
			}
			if !metadataOnly {
				pins, err := repo.Store.Pins()
				if err != nil {
					return err
				}
				for _, path := range pins {
					if err := repo.Fetch(ctx, path, ""); err != nil {
						return err
					}
				}
				dropped, err := repo.Prune(ctx)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.Out, "Synchronized metadata; refreshed %d pin(s); evicted %d file(s)\n", len(pins), len(dropped))
			} else {
				fmt.Fprintln(a.Out, "Synchronized metadata")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&metadataOnly, "metadata-only", false, "skip pin refresh and quota enforcement")
	return cmd
}

func (a *App) statusCommand() *cobra.Command {
	return &cobra.Command{
		Use: "status", Args: cobra.NoArgs, Short: "Show repository and cache status",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			value, err := repo.Status(ctx)
			if err == nil {
				fmt.Fprint(a.Out, value)
			}
			return err
		},
	}
}

func (a *App) fetchCommand() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use: "fetch <path>...", Args: cobra.MinimumNArgs(1), Short: "Download content into the local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			for _, path := range args {
				if err := repo.Fetch(ctx, path, from); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "preferred git-annex remote")
	return cmd
}

func (a *App) pinCommand() *cobra.Command {
	return &cobra.Command{
		Use: "pin <path>...", Args: cobra.MinimumNArgs(1), Short: "Download content and protect it from eviction",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			for _, path := range args {
				if err := repo.Pin(ctx, path); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (a *App) unpinCommand() *cobra.Command {
	return &cobra.Command{
		Use: "unpin <path>...", Args: cobra.MinimumNArgs(1), Short: "Allow content to be evicted",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			for _, path := range args {
				if err := repo.Unpin(path); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (a *App) evictCommand() *cobra.Command {
	return &cobra.Command{
		Use: "evict <path>...", Args: cobra.MinimumNArgs(1), Short: "Remove local content while preserving the namespace entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			for _, path := range args {
				if err := repo.Evict(ctx, path); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func (a *App) cacheCommand() *cobra.Command {
	cache := &cobra.Command{Use: "cache", Short: "Inspect and enforce the local cache"}
	cache.AddCommand(
		&cobra.Command{
			Use: "status", Args: cobra.NoArgs, Short: "Show cache use, limit, and pins",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				usage, err := repo.CacheUsage()
				if err != nil {
					return err
				}
				pins, err := repo.Store.Pins()
				if err != nil {
					return err
				}
				fmt.Fprintf(a.Out, "%s / %s used\n", config.FormatSize(usage), config.FormatSize(repo.Config.CacheLimit))
				for _, path := range pins {
					fmt.Fprintf(a.Out, "pinned\t%s\n", path)
				}
				return nil
			},
		},
		&cobra.Command{
			Use: "set-limit <size>", Args: cobra.ExactArgs(1), Short: "Set the hard local cache target",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				limit, err := config.ParseSize(args[0])
				if err != nil {
					return err
				}
				return repo.SetCacheLimit(limit)
			},
		},
		&cobra.Command{
			Use: "prune", Args: cobra.NoArgs, Short: "Evict LRU content until the cache is within its limit",
			RunE: func(cmd *cobra.Command, args []string) error {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				dropped, err := repo.Prune(ctx)
				if err != nil {
					return err
				}
				for _, path := range dropped {
					fmt.Fprintln(a.Out, path)
				}
				return nil
			},
		},
	)
	return cache
}

func (a *App) historyCommand() *cobra.Command {
	return &cobra.Command{
		Use: "history [path]", Args: cobra.MaximumNArgs(1), Short: "Show namespace history",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			path := ""
			if len(args) == 1 {
				path = args[0]
			}
			ctx, cancel := commandContext(cmd)
			defer cancel()
			value, err := repo.History(ctx, path)
			if err == nil {
				fmt.Fprintln(a.Out, value)
			}
			return err
		},
	}
}

func (a *App) restoreCommand() *cobra.Command {
	return &cobra.Command{
		Use: "restore <revision> [path]", Args: cobra.RangeArgs(1, 2), Short: "Restore a version as a new history commit",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			path := ""
			if len(args) == 2 {
				path = args[1]
			}
			ctx, cancel := commandContext(cmd)
			defer cancel()
			return repo.Restore(ctx, args[0], path)
		},
	}
}

func (a *App) conflictsCommand() *cobra.Command {
	return &cobra.Command{
		Use: "conflicts", Args: cobra.NoArgs, Short: "List unresolved Git namespace conflicts",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			conflicts, err := repo.Conflicts(ctx)
			if err != nil {
				return err
			}
			if len(conflicts) == 0 {
				fmt.Fprintln(a.Out, "No conflicts")
			} else {
				fmt.Fprintln(a.Out, strings.Join(conflicts, "\n"))
			}
			return nil
		},
	}
}

func (a *App) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Args: cobra.NoArgs, Short: "Check build and runtime dependencies",
		RunE: func(cmd *cobra.Command, args []string) error {
			commands := []string{"git", "git-annex", "ssh", "rsync"}
			if runtime.GOOS == "linux" {
				commands = append(commands, "fusermount3")
			}
			failed := false
			for _, name := range commands {
				path, err := exec.LookPath(name)
				if err != nil {
					failed = true
					fmt.Fprintf(a.Out, "MISSING\t%s\n", name)
				} else {
					fmt.Fprintf(a.Out, "OK\t%s\t%s\n", name, path)
				}
			}
			if runtime.GOOS == "linux" {
				if _, err := os.Stat("/dev/fuse"); err != nil {
					failed = true
					fmt.Fprintln(a.Out, "MISSING\t/dev/fuse")
				} else {
					fmt.Fprintln(a.Out, "OK\t/dev/fuse")
				}
			}
			if runtime.GOOS == "darwin" {
				paths := []string{
					"/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse",
					"/Library/Filesystems/osxfuse.fs/Contents/Resources/mount_osxfuse",
				}
				found := ""
				for _, path := range paths {
					if _, err := os.Stat(path); err == nil {
						found = path
						break
					}
				}
				if found == "" {
					failed = true
					fmt.Fprintln(a.Out, "MISSING\tmacFUSE")
				} else {
					fmt.Fprintf(a.Out, "OK\tmacFUSE\t%s\n", found)
				}
			}
			if failed {
				return fmt.Errorf("one or more required commands are missing")
			}
			return nil
		},
	}
}
