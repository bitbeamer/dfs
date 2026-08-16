package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	dfsmount "github.com/bitbeamer/dfs/internal/mount"
	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
	dfssetup "github.com/bitbeamer/dfs/internal/setup"
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
		app.setupCommand(), app.initCommand(), app.joinCommand(), app.peerCommand(), app.networkCommand(), app.pairCommand(), app.relayCommand(),
		app.storageCommand(),
		app.mountCommand(), app.unmountCommand(), app.healthCommand(), app.syncCommand(), app.statusCommand(),
		app.fetchCommand(), app.pinCommand(), app.unpinCommand(), app.evictCommand(),
		app.cacheCommand(), app.historyCommand(), app.restoreCommand(), app.conflictsCommand(),
		app.doctorCommand(),
	)
	return root
}

func (a *App) setupCommand() *cobra.Command {
	var repositoryPath, mountpoint, name, limit, installer string
	var discoveryTimeout time.Duration
	var resume, abort, yes bool
	command := &cobra.Command{
		Use: "setup", Args: cobra.NoArgs,
		Short: "Join, install, mount, and verify DFS as one recoverable transaction",
		RunE: func(cmd *cobra.Command, args []string) error {
			cacheLimit, err := config.ParseSize(limit)
			if err != nil {
				return err
			}
			ctx, cancel := commandContext(cmd)
			defer cancel()
			if abort {
				if err := dfssetup.Abort(ctx, repositoryPath, installer, a.Out); err != nil {
					return err
				}
				fmt.Fprintln(a.Out, "Aborted DFS setup and removed its managed state")
				return nil
			}
			invitation := ""
			reader := bufio.NewReader(cmd.InOrStdin())
			if !resume {
				fmt.Fprint(a.Out, "Paste DFS invitation: ")
				invitation, err = reader.ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return fmt.Errorf("read DFS invitation: %w", err)
				}
				invitation = strings.TrimSpace(invitation)
				if invitation == "" {
					return errors.New("DFS invitation is empty")
				}
			}
			approve := func(state *dfssetup.State) error {
				shortID := state.FileSystemID
				if len(shortID) > 12 {
					shortID = shortID[:12]
				}
				if yes {
					fmt.Fprintf(a.Out, "Approved joining DFS filesystem %s as %s\n", shortID, state.Name)
					return nil
				}
				fmt.Fprintf(a.Out, "Join DFS filesystem %s as %s and install its managed mount? [y/N] ", shortID, state.Name)
				answer, readErr := reader.ReadString('\n')
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return readErr
				}
				answer = strings.ToLower(strings.TrimSpace(answer))
				if answer != "y" && answer != "yes" {
					return errors.New("DFS setup was not approved; resume later with dfs setup --resume")
				}
				return nil
			}
			state, err := dfssetup.Run(ctx, dfssetup.Options{Invitation: invitation, Repository: repositoryPath, Mountpoint: mountpoint,
				Name: name, CacheLimit: cacheLimit, Timeout: discoveryTimeout, Resume: resume, Installer: installer,
				Out: a.Out, Approve: approve})
			if err != nil {
				return fmt.Errorf("DFS setup stopped at a recoverable step: %w (retry with dfs setup --resume or roll back with dfs setup --abort)", err)
			}
			fmt.Fprintf(a.Out, "DFS setup verified: %s is mounted at %s\n", state.NetworkName, state.Mountpoint)
			return nil
		},
	}
	home, _ := os.UserHomeDir()
	command.Flags().StringVar(&repositoryPath, "repository", filepath.Join(home, ".local", "share", "dfs", "repository"), "private DFS repository path")
	command.Flags().StringVar(&mountpoint, "mountpoint", filepath.Join(home, "dfs_storage"), "mounted DFS volume path")
	command.Flags().StringVar(&name, "name", "", "peer name (defaults to hostname)")
	command.Flags().StringVar(&limit, "cache-limit", "100GiB", "maximum local content cache")
	command.Flags().DurationVar(&discoveryTimeout, "timeout", 3*time.Second, "how long to discover the invited network")
	command.Flags().StringVar(&installer, "installer", "", "service installer script (advanced)")
	_ = command.Flags().MarkHidden("installer")
	command.Flags().BoolVar(&resume, "resume", false, "resume the recorded setup transaction")
	command.Flags().BoolVar(&abort, "abort", false, "roll back the recorded setup transaction")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "approve setup without an interactive confirmation")
	command.MarkFlagsMutuallyExclusive("resume", "abort")
	return command
}

func (a *App) networkCommand() *cobra.Command {
	network := &cobra.Command{Use: "network", Short: "Discover and join DFS networks"}
	var discoverTimeout time.Duration
	var asJSON bool
	discover := &cobra.Command{
		Use: "discover", Args: cobra.NoArgs, Short: "Discover DFS networks on the local network",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), discoverTimeout+2*time.Second)
			defer cancel()
			offers, err := peer.Discover(ctx, discoverTimeout)
			if err != nil {
				return err
			}
			networks := peer.GroupOffers(offers)
			if asJSON {
				encoder := json.NewEncoder(a.Out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(networks)
			}
			if len(networks) == 0 {
				fmt.Fprintln(a.Out, "No DFS networks discovered")
				return nil
			}
			fmt.Fprintln(a.Out, "NETWORK\tPEERS\tFILESYSTEM ID\tOFFERED BY")
			for _, network := range networks {
				id := network.FileSystemID
				if len(id) > 12 {
					id = id[:12]
				}
				var names []string
				for _, offer := range network.Offers {
					names = append(names, offer.PeerName)
				}
				fmt.Fprintf(a.Out, "%s\t%d\t%s\t%s\n", network.NetworkName, len(network.Offers), id, strings.Join(names, ", "))
			}
			return nil
		},
	}
	discover.Flags().DurationVar(&discoverTimeout, "timeout", 2*time.Second, "how long to listen for local advertisements")
	discover.Flags().BoolVar(&asJSON, "json", false, "emit discovered services as JSON")

	var name, limit string
	var joinTimeout time.Duration
	var noReverse bool
	join := &cobra.Command{
		Use: "join <invitation> <repository>", Args: cobra.ExactArgs(2),
		Short: "Join a discovered DFS network with a pairing invitation",
		RunE: func(cmd *cobra.Command, args []string) error {
			bytes, err := config.ParseSize(limit)
			if err != nil {
				return err
			}
			ctx, cancel := commandContext(cmd)
			defer cancel()
			result, err := peer.PairAndJoin(ctx, args[0], args[1], name, bytes, joinTimeout, !noReverse)
			if err != nil {
				return err
			}
			defer result.Repository.Close()
			fmt.Fprintf(a.Out, "Joined DFS network %s through %s as %s at %s\n",
				result.NetworkName, result.OfferingPeer, result.Repository.Config.Name, result.Repository.Config.Repository)
			if result.ReverseRemoteName != "" {
				fmt.Fprintf(a.Out, "Configured reciprocal peer %s\n", result.ReverseRemoteName)
			}
			return nil
		},
	}
	join.Flags().StringVar(&name, "name", "", "peer name (defaults to hostname)")
	join.Flags().StringVar(&limit, "cache-limit", "100GiB", "maximum local content cache")
	join.Flags().DurationVar(&joinTimeout, "timeout", 3*time.Second, "how long to discover the invited network")
	join.Flags().BoolVar(&noReverse, "no-reverse", false, "do not register this device as a direct source on the approving peer")

	setName := &cobra.Command{
		Use: "set-name <name>", Args: cobra.ExactArgs(1), Short: "Set the local display name advertised for this DFS network",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			return repo.SetNetworkName(args[0])
		},
	}
	complete := &cobra.Command{
		Use: "complete", Args: cobra.NoArgs, Short: "Retry completion of an interrupted peer pairing",
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := config.ResolveRepository(a.repo)
			if err != nil {
				return err
			}
			ctx, cancel := commandContext(cmd)
			defer cancel()
			result, err := peer.CompletePairing(ctx, repositoryPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Completed reciprocal DFS pairing as %s\n", result.RemoteName)
			return nil
		},
	}
	network.AddCommand(discover, join, complete, setName)
	return network
}

func (a *App) pairCommand() *cobra.Command {
	pairing := &cobra.Command{Use: "pair", Short: "Manage secure peer-pairing invitations"}
	var expires time.Duration
	var cloneURL string
	invite := &cobra.Command{
		Use: "invite", Args: cobra.NoArgs, Short: "Create a one-use pairing invitation",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			invitation, err := peer.CreateInvitation(repo, expires, cloneURL)
			if err != nil {
				return err
			}
			encoded, err := invitation.Encode()
			if err != nil {
				return err
			}
			fmt.Fprintln(a.Out, encoded)
			return nil
		},
	}
	invite.Flags().DurationVar(&expires, "expires", 10*time.Minute, "invitation lifetime (maximum 24h)")
	invite.Flags().StringVar(&cloneURL, "clone-url", "", "override the SSH clone URL returned after authentication")

	list := &cobra.Command{
		Use: "list", Args: cobra.NoArgs, Short: "List active pairing invitations",
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := config.ResolveRepository(a.repo)
			if err != nil {
				return err
			}
			invitations, err := peer.ListInvitations(repositoryPath, time.Now())
			if err != nil {
				return err
			}
			if len(invitations) == 0 {
				fmt.Fprintln(a.Out, "No active pairing invitations")
				return nil
			}
			for _, invitation := range invitations {
				state := "available"
				if invitation.Pending {
					state = "pairing"
				}
				fmt.Fprintf(a.Out, "%s\t%s\t%s\n", invitation.ID, state, invitation.ExpiresAt.Format(time.RFC3339))
			}
			return nil
		},
	}
	revoke := &cobra.Command{
		Use: "revoke <invitation-id>", Args: cobra.ExactArgs(1), Short: "Revoke an unused or pending pairing invitation",
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := config.ResolveRepository(a.repo)
			if err != nil {
				return err
			}
			return peer.RevokeInvitation(repositoryPath, args[0])
		},
	}
	pairing.AddCommand(invite, list, revoke)
	return pairing
}

func Execute() error { return New().Execute() }

func (a *App) open() (*repository.Repository, error) { return repository.Open(a.repo) }

func commandContext(command *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(command.Context(), 24*time.Hour)
}

func (a *App) initCommand() *cobra.Command {
	var name, networkName, limit, relay string
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
			if networkName != "" {
				if err := repo.SetNetworkName(networkName); err != nil {
					return err
				}
			}
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
	cmd.Flags().StringVar(&networkName, "network-name", "", "display name advertised for this DFS network")
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
	peers := &cobra.Command{Use: "peer", Short: "Manage direct Git/git-annex peers"}
	serve := &cobra.Command{
		Use: "serve", Args: cobra.NoArgs, Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := config.ResolveRepository(a.repo)
			if err != nil {
				return err
			}
			return peer.ServeSSH(repositoryPath)
		},
	}
	peers.AddCommand(
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
				if strings.HasPrefix(args[0], "dfs-peer-") {
					if err := peer.RevokeMembership(ctx, repo, args[0]); err != nil {
						return err
					}
				}
				if err := repo.RemovePeer(ctx, args[0]); err != nil {
					return err
				}
				return peer.RevokePeerAuthorization(args[0])
			},
		}, serve,
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
	return peers
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
	var logLevel, logFormat, logFile string
	var pairingPort int
	var discovery bool
	var fuseDebug, recoverStaleSession, managed bool
	cmd := &cobra.Command{
		Use: "mount <mountpoint>", Args: cobra.ExactArgs(1), Short: "Mount the DFS namespace and run automatic sync",
		RunE: func(cmd *cobra.Command, args []string) error {
			mountSignals := make(chan os.Signal, 2)
			signal.Notify(mountSignals, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(mountSignals)
			logger, closer, err := newMountLogger(logLevel, logFormat, logFile, a.Err, fuseDebug)
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
			if !managed {
				fmt.Fprintf(a.Out, "Mounting %s at %s; press Ctrl-C to stop\n", repo.Config.Repository, args[0])
			}
			return dfsmount.Run(repo, args[0], dfsmount.Options{
				Context: cmd.Context(), Logger: logger, FUSEDebug: fuseDebug,
				RecoverStaleSession: recoverStaleSession, Signals: mountSignals, PairingPort: pairingPort,
				DisablePeerDiscovery: !discovery,
			})
		},
	}
	cmd.Flags().StringVar(&logLevel, "log-level", "error", "logging level: debug, info, warn, or error")
	cmd.Flags().StringVar(&logFormat, "log-format", "text", "structured log format: text or json")
	cmd.Flags().StringVar(&logFile, "log-file", "", "append logs to this file as well as stderr")
	cmd.Flags().BoolVar(&fuseDebug, "fuse-debug", false, "log low-level FUSE protocol requests and enable debug logging (very noisy)")
	cmd.Flags().BoolVar(&recoverStaleSession, "recover-stale-session", false, "take over after verifying another host's recorded mount is inactive")
	cmd.Flags().BoolVar(&managed, "managed", false, "run under a service manager without interactive output")
	cmd.Flags().IntVar(&pairingPort, "pair-port", peer.DefaultPairingPort, "TCP port for authenticated peer pairing")
	cmd.Flags().BoolVar(&discovery, "discovery", true, "advertise this DFS network and accept authenticated pairing")
	return cmd
}

func (a *App) unmountCommand() *cobra.Command {
	return &cobra.Command{
		Use: "unmount <mountpoint>", Aliases: []string{"umount"}, Args: cobra.ExactArgs(1), Short: "Unmount a DFS namespace",
		RunE: func(cmd *cobra.Command, args []string) error { return dfsmount.Unmount(args[0]) },
	}
}

func (a *App) healthCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "health", Args: cobra.NoArgs, Short: "Report managed mount health",
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := config.ResolveRepository(a.repo)
			if err != nil {
				return err
			}
			report, healthErr := dfsmount.CheckHealth(repositoryPath)
			if asJSON {
				encoder := json.NewEncoder(a.Out)
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(report); err != nil {
					return err
				}
			} else if report.Version != 0 {
				fmt.Fprintf(a.Out, "%s: peer %s mounted at %s (pid %d, updated %s)\n",
					report.State, report.Peer, report.Mountpoint, report.PID, report.UpdatedAt.Format(time.RFC3339))
			}
			return healthErr
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the complete health report as JSON")
	return cmd
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
	var mesh bool
	var discoveryTimeout, peerTimeout time.Duration
	cmd := &cobra.Command{
		Use: "doctor", Args: cobra.NoArgs, Short: "Check build and runtime dependencies",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := prepareDoctorPath(runtime.GOOS); err != nil {
				return fmt.Errorf("prepare dependency search path: %w", err)
			}
			commands := []string{"git", "git-annex", "git-annex-shell", "ssh", "ssh-keygen", "rsync"}
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
			if mesh {
				repo, err := a.open()
				if err != nil {
					return err
				}
				defer repo.Close()
				ctx, cancel := commandContext(cmd)
				defer cancel()
				report, err := peer.CheckMesh(ctx, repo, discoveryTimeout, peerTimeout)
				if err != nil {
					return fmt.Errorf("check peer mesh: %w", err)
				}
				printMeshReport(a.Out, report)
				if !report.Complete {
					return fmt.Errorf("peer mesh is incomplete or unreachable")
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&mesh, "mesh", false, "check every directed connection between configured mesh peers")
	cmd.Flags().DurationVar(&discoveryTimeout, "discovery-timeout", 2*time.Second, "how long to discover peers for the mesh check")
	cmd.Flags().DurationVar(&peerTimeout, "peer-timeout", 10*time.Second, "maximum time for each peer connection probe")
	return cmd
}

func prepareDoctorPath(goos string) error {
	if goos != "darwin" {
		return nil
	}
	current := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
	seen := make(map[string]bool, len(current)+2)
	for _, directory := range current {
		seen[directory] = true
	}
	result := make([]string, 0, len(current)+2)
	for _, directory := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !seen[directory] {
			result = append(result, directory)
			seen[directory] = true
		}
	}
	result = append(result, current...)
	return os.Setenv("PATH", strings.Join(result, string(os.PathListSeparator)))
}

func printMeshReport(output io.Writer, report peer.MeshReport) {
	if len(report.Peers) == 1 {
		fmt.Fprintf(output, "MESH\tONLY_LOCAL_PEER\t%s\n", meshPeerLabel(report.Peers[0]))
		return
	}
	names := make(map[string]string, len(report.Peers))
	for _, participant := range report.Peers {
		names[participant.PeerID] = meshPeerLabel(participant)
	}
	fmt.Fprintln(output, "FROM\tTO\tSTATUS\tDETAIL")
	for _, connection := range report.Connections {
		fmt.Fprintf(output, "%s\t%s\t%s\t%s\n",
			names[connection.FromPeerID], names[connection.ToPeerID], connection.Status, connection.Error)
	}
}

func meshPeerLabel(participant peer.MeshPeer) string {
	id := participant.PeerID
	if len(id) > 12 {
		id = id[:12]
	}
	if participant.PeerName == "" || participant.PeerName == participant.PeerID {
		return id
	}
	return participant.PeerName + " (" + id + ")"
}
