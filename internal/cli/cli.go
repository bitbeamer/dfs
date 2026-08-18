package cli

import (
	"bufio"
	"bytes"
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
	"text/tabwriter"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	dfscore "github.com/bitbeamer/dfs/internal/daemon"
	dfsinstance "github.com/bitbeamer/dfs/internal/instance"
	"github.com/bitbeamer/dfs/internal/managed"
	dfsmount "github.com/bitbeamer/dfs/internal/mount"
	"github.com/bitbeamer/dfs/internal/optimization"
	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
	dfssetup "github.com/bitbeamer/dfs/internal/setup"
	"github.com/bitbeamer/dfs/internal/wakeup"
	"github.com/spf13/cobra"
)

var Version = "dev"

type App struct {
	Out          io.Writer
	Err          io.Writer
	repo         string
	filesystem   string
	output       string
	quiet        bool
	yes          bool
	destination  io.Writer
	capture      bytes.Buffer
	capturing    bool
	filesystemID string
}

func New() *cobra.Command {
	root, _ := newRoot()
	return root
}

func newRoot() (*cobra.Command, *App) {
	app := &App{Out: os.Stdout, Err: os.Stderr, destination: os.Stdout}
	root := &cobra.Command{
		Use:           "dfs",
		Short:         "A quota-aware distributed filesystem built on Git and git-annex",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.SetOut(app.Out)
	root.SetErr(app.Err)
	root.PersistentFlags().StringVar(&app.filesystem, "filesystem", "", "filesystem name, ID, mountpoint, or repository")
	root.PersistentFlags().StringVar(&app.output, "output", "text", "output format: text or json")
	root.PersistentFlags().BoolVar(&app.quiet, "quiet", false, "suppress successful text output")
	root.PersistentFlags().BoolVarP(&app.yes, "yes", "y", false, "approve confirmed operations non-interactively")
	root.PersistentFlags().StringVar(&app.repo, "repo", "", "DFS repository (internal recovery override)")
	_ = root.PersistentFlags().MarkHidden("repo")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		app.filesystemID = ""
		if app.output != "text" && app.output != "json" {
			return fmt.Errorf("unsupported output format %q; use text or json", app.output)
		}
		if app.output == "json" {
			if flag := cmd.Flags().Lookup("json"); flag != nil {
				if err := flag.Value.Set("true"); err != nil {
					return err
				}
			}
			app.capture.Reset()
			app.Out = &app.capture
			app.capturing = true
		} else if app.quiet {
			app.Out = io.Discard
		}
		return nil
	}
	root.PersistentPostRunE = func(cmd *cobra.Command, _ []string) error {
		defer func() {
			app.Out = app.destination
			app.capturing = false
		}()
		if !app.capturing {
			return nil
		}
		result := app.capturedResult()
		scope := "filesystem"
		path := cmd.CommandPath()
		if strings.HasPrefix(path, "dfs service ") || path == "dfs upgrade" {
			scope = "host"
		} else if value, err := cmd.Flags().GetString("scope"); err == nil {
			scope = value
		}
		envelope := map[string]any{"schema": "dfs.cli/v1", "command": path, "scope": scope, "result": result}
		if app.filesystemID != "" {
			envelope["filesystem_id"] = app.filesystemID
		}
		return json.NewEncoder(app.destination).Encode(envelope)
	}
	root.AddCommand(app.publicCommands()...)
	root.AddCommand(app.internalCommand())
	root.AddCommand(app.legacyRuntimeCommands()...)
	return root, app
}

func (a *App) capturedResult() any {
	trimmed := bytes.TrimSpace(a.capture.Bytes())
	if len(trimmed) == 0 {
		return nil
	}
	var result any
	if err := json.Unmarshal(trimmed, &result); err != nil {
		return map[string]any{"text": string(trimmed)}
	}
	return result
}

func (a *App) instanceCommand() *cobra.Command {
	instancesCommand := &cobra.Command{Use: "instance", Short: "Administer every managed DFS filesystem on this host"}
	var listJSON bool
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List installed DFS filesystem instances",
		RunE: func(cmd *cobra.Command, _ []string) error {
			instances, err := dfsinstance.Discover(cmd.Context())
			if err != nil {
				return err
			}
			if listJSON {
				encoder := json.NewEncoder(a.Out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(instances)
			}
			if len(instances) == 0 {
				fmt.Fprintln(a.Out, "No managed DFS filesystem instances found")
				return nil
			}
			writer := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(writer, "FILESYSTEM\tPEER\tID\tSTATUS\tPORT\tMOUNTPOINT\tREPOSITORY")
			for _, instance := range instances {
				id := instance.FileSystemID
				if len(id) > 12 {
					id = id[:12]
				}
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", instance.NetworkName, instance.Name, id, instanceStatus(instance), instance.PairingPort, instance.Mountpoint, instance.Repository)
			}
			return writer.Flush()
		}}
	list.Flags().BoolVar(&listJSON, "json", false, "emit machine-readable instance data")

	var stopAll bool
	stop := &cobra.Command{Use: "stop [name|id|repository]", Args: cobra.MaximumNArgs(1), Short: "Stop one or every managed DFS filesystem without uninstalling it",
		RunE: func(cmd *cobra.Command, args []string) error {
			instances, err := dfsinstance.Discover(cmd.Context())
			if err != nil {
				return err
			}
			selected, err := selectManagedInstances(instances, args, stopAll, a.filesystem)
			if err != nil {
				return err
			}
			if err := dfsinstance.Stop(cmd.Context(), selected); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Stopped %d DFS filesystem instance(s)\n", len(selected))
			return nil
		}}
	stop.Flags().BoolVar(&stopAll, "all", false, "stop every managed DFS filesystem")

	var updateBinary, updateInstaller string
	update := &cobra.Command{Use: "update", Args: cobra.NoArgs, Short: "Update every managed DFS filesystem and preserve its running state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			instances, err := dfsinstance.Discover(cmd.Context())
			if err != nil {
				return err
			}
			if len(instances) == 0 {
				return errors.New("no managed DFS filesystem instances found")
			}
			binary := updateBinary
			if binary == "" {
				binary, err = os.Executable()
				if err != nil {
					return err
				}
			}
			if err := dfsinstance.Update(cmd.Context(), instances, binary, updateInstaller); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Updated %d DFS filesystem instance(s)\n", len(instances))
			return nil
		}}
	update.Flags().StringVar(&updateBinary, "binary", "", "replacement DFS executable (defaults to this executable)")
	update.Flags().StringVar(&updateInstaller, "installer", "", "service installer script (advanced)")
	_ = update.Flags().MarkHidden("installer")

	var uninstallAll, uninstallYes bool
	uninstall := &cobra.Command{Use: "uninstall [name|id|repository]", Args: cobra.MaximumNArgs(1), Short: "Remove managed services while retaining DFS repository data",
		RunE: func(cmd *cobra.Command, args []string) error {
			instances, err := dfsinstance.Discover(cmd.Context())
			if err != nil {
				return err
			}
			selected, err := selectManagedInstances(instances, args, uninstallAll, a.filesystem)
			if err != nil {
				return err
			}
			if uninstallAll && !uninstallYes {
				return errors.New("uninstalling every DFS instance requires --yes; repository data will be retained")
			}
			if err := dfsinstance.Uninstall(cmd.Context(), selected); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Uninstalled %d DFS filesystem instance(s); repository data was retained\n", len(selected))
			return nil
		}}
	uninstall.Flags().BoolVar(&uninstallAll, "all", false, "uninstall every managed DFS filesystem")
	uninstall.Flags().BoolVarP(&uninstallYes, "yes", "y", false, "confirm host-wide uninstall")

	instancesCommand.AddCommand(list, stop, update, uninstall)
	return instancesCommand
}

func selectManagedInstances(instances []dfsinstance.Instance, args []string, all bool, implicit ...string) ([]dfsinstance.Instance, error) {
	selector := ""
	if len(implicit) > 0 {
		selector = strings.TrimSpace(implicit[0])
	}
	if selector != "" {
		if len(args) != 0 {
			return nil, errors.New("pass the service selector as an argument or --filesystem, not both")
		}
		args = []string{selector}
	}
	if all {
		if len(args) != 0 {
			return nil, errors.New("pass either one DFS instance selector or --all, not both")
		}
		if len(instances) == 0 {
			return nil, errors.New("no managed DFS filesystem instances found")
		}
		return instances, nil
	}
	if len(args) != 1 {
		return nil, errors.New("pass one DFS instance name, ID, or repository, or use --all")
	}
	selected, err := dfsinstance.Find(instances, args[0])
	if err != nil {
		return nil, err
	}
	return []dfsinstance.Instance{selected}, nil
}

func instanceStatus(instance dfsinstance.Instance) string {
	if instance.CoreActive && instance.MountActive {
		return "RUNNING"
	}
	if instance.Active() {
		return "PARTIAL"
	}
	return "STOPPED"
}

func (a *App) transportCommand() *cobra.Command {
	transport := &cobra.Command{Use: "transport", Short: "Inspect the DFS-managed QUIC transport"}
	gitProxy := &cobra.Command{Use: "git <peer-id> <service>", Args: cobra.ExactArgs(2), Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			_, err = managed.GitProxy(ctx, repo, args[0], args[1], cmd.InOrStdin(), a.Out, a.Err)
			return err
		}}
	probe := &cobra.Command{Use: "probe <peer-id>", Args: cobra.ExactArgs(1), Short: "Probe mutually authenticated QUIC connectivity",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			if err := managed.Probe(ctx, repo, args[0]); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Managed QUIC transport is reachable and authenticated")
			return nil
		}}
	transport.AddCommand(gitProxy, probe)
	return transport
}

func (a *App) setupCommand() *cobra.Command {
	var repositoryPath, mountpoint, name, limit, installer, filesystem, networkName, gitName, gitEmail string
	var pairingPort int
	var discoveryTimeout, verificationTimeout time.Duration
	var resume, abort, yes, create bool
	command := &cobra.Command{
		Use: "setup", Args: cobra.NoArgs,
		Short: "Create or join, install, mount, and verify DFS as one recoverable transaction",
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
			reader := bufio.NewReader(cmd.InOrStdin())
			selectedFilesystem := strings.TrimSpace(filesystem)
			selectedNetworkName := ""
			if !resume && !create {
				selectedFilesystem, selectedNetworkName, err = selectDiscoveredFilesystem(cmd.Context(), reader, a.Out, selectedFilesystem, discoveryTimeout)
				if err != nil {
					return err
				}
			}
			if create {
				selectedNetworkName = networkName
			}
			gitName, gitEmail, err = ensureGitIdentity(cmd.Context(), reader, a.Out, gitName, gitEmail, yes)
			if err != nil {
				return err
			}
			approve := func(state *dfssetup.State) error {
				if state.Create {
					if yes {
						fmt.Fprintf(a.Out, "Approved creating DFS filesystem %q as %s on managed port %d\n", state.NetworkName, state.Name, state.PairingPort)
						return nil
					}
					fmt.Fprintf(a.Out, "Create DFS filesystem %q as %s and install its managed mount on port %d? [y/N] ", state.NetworkName, state.Name, state.PairingPort)
				} else {
					filesystemName := setupFilesystemName(state)
					if yes {
						fmt.Fprintf(a.Out, "Approved joining DFS filesystem %q as %s on managed port %d\n", filesystemName, state.Name, state.PairingPort)
						return nil
					}
					fmt.Fprintf(a.Out, "Join DFS filesystem %q as %s and install its managed mount on port %d? [y/N] ", filesystemName, state.Name, state.PairingPort)
				}
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
			state, err := dfssetup.Run(ctx, dfssetup.Options{FileSystemID: selectedFilesystem, Create: create, NetworkName: selectedNetworkName, Repository: repositoryPath, Mountpoint: mountpoint,
				Name: name, GitName: gitName, GitEmail: gitEmail, CacheLimit: cacheLimit, Timeout: discoveryTimeout, VerificationTimeout: verificationTimeout, Resume: resume, Installer: installer,
				PairingPort: pairingPort, Out: a.Out, Approve: approve})
			if err != nil {
				return fmt.Errorf("DFS setup stopped at a recoverable step: %w (retry with dfs setup --resume or roll back with dfs setup --abort)", err)
			}
			fmt.Fprintf(a.Out, "DFS setup verified: %s is mounted at %s on managed port %d\n", state.NetworkName, state.Mountpoint, state.PairingPort)
			return nil
		},
	}
	home, _ := os.UserHomeDir()
	defaultRepository, _ := config.DefaultRepositoryPath()
	command.Flags().StringVar(&repositoryPath, "repository", defaultRepository, "private DFS repository path")
	command.Flags().StringVar(&mountpoint, "mountpoint", filepath.Join(home, "dfs_storage"), "mounted DFS volume path")
	command.Flags().StringVar(&name, "name", "", "peer name (defaults to hostname)")
	command.Flags().StringVar(&limit, "cache-limit", "100GiB", "maximum local content cache")
	command.Flags().DurationVar(&discoveryTimeout, "timeout", 3*time.Second, "how long to discover the invited network")
	command.Flags().DurationVar(&verificationTimeout, "verification-timeout", time.Minute, "how long online members have to acknowledge the cluster topology")
	command.Flags().IntVar(&pairingPort, "pair-port", 0, "local managed transport port (defaults to the first free port from 7843)")
	command.Flags().StringVar(&installer, "installer", "", "service installer script (advanced)")
	_ = command.Flags().MarkHidden("installer")
	command.Flags().BoolVar(&resume, "resume", false, "resume the recorded setup transaction")
	command.Flags().BoolVar(&abort, "abort", false, "roll back the recorded setup transaction")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "approve setup without an interactive confirmation")
	command.Flags().StringVar(&filesystem, "filesystem", "", "discovered filesystem ID or unambiguous name")
	command.Flags().BoolVar(&create, "create", false, "create the first peer of a new DFS filesystem")
	command.Flags().StringVar(&networkName, "network-name", "", "display name for a newly created DFS filesystem (defaults to the mountpoint name)")
	command.Flags().StringVar(&gitName, "git-name", "", "Git author name stored in this DFS repository")
	command.Flags().StringVar(&gitEmail, "git-email", "", "Git author email stored in this DFS repository")
	command.MarkFlagsMutuallyExclusive("resume", "abort")
	command.MarkFlagsMutuallyExclusive("create", "filesystem")
	return command
}

func selectDiscoveredFilesystem(ctx context.Context, reader *bufio.Reader, out io.Writer, wanted string, timeout time.Duration) (string, string, error) {
	offers, err := discoverSetupNetworks(ctx, out, timeout, time.Second, peer.Discover)
	if err != nil {
		return "", "", err
	}
	offers = peer.ReachableOffers(ctx, offers, time.Second)
	networks := peer.GroupOffers(offers)
	if len(networks) == 0 {
		return "", "", errors.New("no DFS filesystems discovered")
	}
	if wanted != "" {
		var matches []peer.Network
		for _, network := range networks {
			if network.FileSystemID == wanted || strings.EqualFold(network.NetworkName, wanted) || strings.HasPrefix(network.FileSystemID, wanted) {
				matches = append(matches, network)
			}
		}
		if len(matches) != 1 {
			return "", "", fmt.Errorf("filesystem %q does not identify exactly one discovered DFS filesystem", wanted)
		}
		return matches[0].FileSystemID, matches[0].NetworkName, nil
	}
	fmt.Fprintln(out, "Discovered DFS filesystems:")
	for index, network := range networks {
		id := network.FileSystemID
		if len(id) > 12 {
			id = id[:12]
		}
		fmt.Fprintf(out, "  %d) %s (%s, %d online peer(s))\n", index+1, network.NetworkName, id, len(network.Offers))
	}
	if len(networks) == 1 {
		fmt.Fprintln(out, "Selected the only discovered filesystem")
		return networks[0].FileSystemID, networks[0].NetworkName, nil
	}
	fmt.Fprintf(out, "Select filesystem [1-%d]: ", len(networks))
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	var selected int
	if _, err := fmt.Sscanf(strings.TrimSpace(answer), "%d", &selected); err != nil || selected < 1 || selected > len(networks) {
		return "", "", errors.New("invalid DFS filesystem selection")
	}
	return networks[selected-1].FileSystemID, networks[selected-1].NetworkName, nil
}

func setupFilesystemName(state *dfssetup.State) string {
	if name := strings.TrimSpace(state.NetworkName); name != "" {
		return name
	}
	id := state.FileSystemID
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func ensureGitIdentity(ctx context.Context, reader *bufio.Reader, out io.Writer, name, email string, nonInteractive bool) (string, string, error) {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("GIT_AUTHOR_NAME"))
	}
	if email == "" {
		email = strings.TrimSpace(os.Getenv("GIT_AUTHOR_EMAIL"))
	}
	if name == "" {
		name = globalGitConfig(ctx, "user.name")
	}
	if email == "" {
		email = globalGitConfig(ctx, "user.email")
	}
	if nonInteractive && (name == "" || email == "") {
		return "", "", errors.New("Git author identity is required; pass --git-name and --git-email or configure user.name and user.email globally")
	}
	if name == "" {
		fmt.Fprint(out, "Git author name (stored only in this DFS filesystem): ")
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", err
		}
		name = strings.TrimSpace(value)
	}
	if email == "" {
		fmt.Fprint(out, "Git author email (stored only in this DFS filesystem): ")
		value, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", err
		}
		email = strings.TrimSpace(value)
	}
	if name == "" {
		return "", "", errors.New("Git author name cannot be empty")
	}
	if !validGitEmail(email) {
		return "", "", fmt.Errorf("Git author email %q is invalid", email)
	}
	fmt.Fprintf(out, "Git author identity: %s <%s>\n", name, email)
	return name, email, nil
}

func globalGitConfig(ctx context.Context, key string) string {
	command := exec.CommandContext(ctx, "git", "config", "--global", "--get", key)
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func validGitEmail(value string) bool {
	value = strings.TrimSpace(value)
	at := strings.LastIndexByte(value, '@')
	return at > 0 && at < len(value)-1 && !strings.ContainsAny(value, " \t\r\n<>")
}

type setupDiscoverer func(context.Context, time.Duration) ([]peer.Offer, error)

type setupDiscoveryResult struct {
	offers []peer.Offer
	err    error
}

func discoverSetupNetworks(ctx context.Context, out io.Writer, timeout, progressInterval time.Duration, discover setupDiscoverer) ([]peer.Offer, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if progressInterval <= 0 {
		progressInterval = time.Second
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	fmt.Fprintf(out, "Searching for DFS filesystems on the local network (up to %s)...\n", timeout)
	result := make(chan setupDiscoveryResult, 1)
	go func() {
		offers, err := discover(discoveryCtx, timeout)
		result <- setupDiscoveryResult{offers: offers, err: err}
	}()
	started := time.Now()
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case found := <-result:
			if found.err == nil {
				fmt.Fprintf(out, "Discovery finished: %d network offer(s) found.\n", len(found.offers))
			}
			return found.offers, found.err
		case <-ticker.C:
			fmt.Fprintf(out, "Still searching for DFS filesystems... %s elapsed\n", time.Since(started).Round(time.Second))
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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
			repositoryPath, err := a.resolveRepository()
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
	pairing := &cobra.Command{Use: "pair", Short: "Review and approve secure peer join requests"}
	var expires time.Duration
	invite := &cobra.Command{
		Use: "invite", Args: cobra.NoArgs, Short: "Create a one-use pairing invitation",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			invitation, err := peer.CreateInvitation(repo, expires)
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

	list := &cobra.Command{
		Use: "list", Args: cobra.NoArgs, Short: "List active pairing invitations",
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := a.resolveRepository()
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
			repositoryPath, err := a.resolveRepository()
			if err != nil {
				return err
			}
			return peer.RevokeInvitation(repositoryPath, args[0])
		},
	}
	requests := &cobra.Command{Use: "requests", Args: cobra.NoArgs, Short: "List pending peer join requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := a.resolveRepository()
			if err != nil {
				return err
			}
			pending, err := peer.ListJoinRequests(repositoryPath, time.Now())
			if err != nil {
				return err
			}
			if len(pending) == 0 {
				fmt.Fprintln(a.Out, "No pending DFS join requests")
				return nil
			}
			fmt.Fprintln(a.Out, "REQUEST\tPEER\tPEER ID\tSTATUS\tEXPIRES")
			for _, request := range pending {
				status := "pending"
				if request.Approved {
					status = "approved"
				}
				fmt.Fprintf(a.Out, "%s\t%s\t%s\t%s\t%s\n", request.ID, request.PeerName, request.PeerID, status, request.ExpiresAt.Format(time.RFC3339))
			}
			return nil
		}}
	approve := &cobra.Command{Use: "approve <request-id>", Args: cobra.ExactArgs(1), Short: "Approve an authenticated pending peer",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			if _, err := peer.ApproveJoinRequest(repo, args[0], 10*time.Minute); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Approved DFS join request %s\n", args[0])
			return nil
		}}
	reject := &cobra.Command{Use: "reject <request-id>", Args: cobra.ExactArgs(1), Short: "Reject a pending peer join request",
		RunE: func(cmd *cobra.Command, args []string) error {
			repositoryPath, err := a.resolveRepository()
			if err != nil {
				return err
			}
			if err := peer.RejectJoinRequest(repositoryPath, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "Rejected DFS join request %s\n", args[0])
			return nil
		}}
	pairing.AddCommand(requests, approve, reject, invite, list, revoke)
	return pairing
}

type JSONCommandError struct{ Err error }

func (e *JSONCommandError) Error() string { return e.Err.Error() }
func (e *JSONCommandError) Unwrap() error { return e.Err }

func Execute() error {
	root, app := newRoot()
	err := root.Execute()
	if err == nil {
		return nil
	}
	output, _ := root.PersistentFlags().GetString("output")
	if output != "json" {
		return err
	}
	command := publicCommandPath(root, os.Args[1:])
	scope := "filesystem"
	if strings.HasPrefix(command, "dfs service ") || command == "dfs service" || command == "dfs upgrade" {
		scope = "host"
	}
	envelope := app.jsonErrorEnvelope(command, scope, err)
	_ = json.NewEncoder(app.destination).Encode(envelope)
	return &JSONCommandError{Err: err}
}

func (a *App) jsonErrorEnvelope(command, scope string, err error) map[string]any {
	envelope := map[string]any{"schema": "dfs.cli/v1", "command": command, "scope": scope, "error": map[string]any{"code": errorCode(err), "message": err.Error()}}
	if result := a.capturedResult(); result != nil {
		envelope["result"] = result
	}
	if a.filesystemID != "" {
		envelope["filesystem_id"] = a.filesystemID
	}
	return envelope
}

func publicCommandPath(root *cobra.Command, arguments []string) string {
	current := root
	parts := []string{root.Name()}
	for _, argument := range arguments {
		if strings.HasPrefix(argument, "-") {
			continue
		}
		for _, child := range current.Commands() {
			if argument == child.Name() {
				parts = append(parts, child.Name())
				current = child
				break
			}
		}
	}
	return strings.Join(parts, " ")
}

func errorCode(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "required"), strings.Contains(message, "requires"), strings.Contains(message, "unknown command"), strings.Contains(message, "unsupported"):
		return "INVALID_ARGUMENT"
	case strings.Contains(message, "not found"), strings.Contains(message, "no dfs"):
		return "NOT_FOUND"
	case strings.Contains(message, "not approved"):
		return "NOT_CONFIRMED"
	case strings.Contains(message, "health is degraded"):
		return "HEALTH_DEGRADED"
	default:
		return "OPERATION_FAILED"
	}
}

func (a *App) open() (*repository.Repository, error) {
	repositoryPath, err := a.resolveRepository()
	if err != nil {
		return nil, err
	}
	repo, err := repository.Open(repositoryPath)
	if err == nil {
		repo.SetManagedFetcher(managed.FetchPath)
		a.filesystemID = repo.Config.FileSystemID
	}
	return repo, err
}

func (a *App) resolveRepository() (string, error) {
	if a.repo != "" || os.Getenv("DFS_REPO") != "" {
		return config.ResolveRepository(a.repo)
	}
	selector := strings.TrimSpace(a.filesystem)
	if selector == "" {
		selector = strings.TrimSpace(os.Getenv("DFS_FILESYSTEM"))
	}
	instances, discoverErr := dfsinstance.Discover(context.Background())
	if selector != "" {
		if discoverErr != nil {
			return "", discoverErr
		}
		instance, err := dfsinstance.Find(instances, selector)
		if err != nil {
			return "", err
		}
		return instance.Repository, nil
	}
	current, cwdErr := os.Getwd()
	if cwdErr == nil {
		for _, instance := range instances {
			for _, base := range []string{instance.Repository, instance.Mountpoint} {
				relative, err := filepath.Rel(base, current)
				if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					return instance.Repository, nil
				}
			}
		}
	}
	if discoverErr == nil {
		switch len(instances) {
		case 1:
			return instances[0].Repository, nil
		case 0:
			// Manual and developer repositories are still resolved below.
		default:
			var choices []string
			for _, instance := range instances {
				choices = append(choices, fmt.Sprintf("%s (%s)", instance.NetworkName, shortID(instance.FileSystemID)))
			}
			return "", fmt.Errorf("multiple DFS filesystems are installed; select one with --filesystem: %s", strings.Join(choices, ", "))
		}
	}
	return config.ResolveRepository("")
}

func shortID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

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
	peers := &cobra.Command{Use: "peer", Short: "Manage authenticated DFS peers"}
	peers.AddCommand(
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
				return nil
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
				RecoverStaleSession: recoverStaleSession, Signals: mountSignals,
			})
		},
	}
	cmd.Flags().StringVar(&logLevel, "log-level", "error", "logging level: debug, info, warn, or error")
	cmd.Flags().StringVar(&logFormat, "log-format", "text", "structured log format: text or json")
	cmd.Flags().StringVar(&logFile, "log-file", "", "append logs to this file as well as stderr")
	cmd.Flags().BoolVar(&fuseDebug, "fuse-debug", false, "log low-level FUSE protocol requests and enable debug logging (very noisy)")
	cmd.Flags().BoolVar(&recoverStaleSession, "recover-stale-session", false, "take over after verifying another host's recorded mount is inactive")
	cmd.Flags().BoolVar(&managed, "managed", false, "run under a service manager without interactive output")
	return cmd
}

func (a *App) daemonCommand() *cobra.Command {
	var logLevel, logFormat, logFile, mountpoint string
	var pairingPort int
	var managed bool
	cmd := &cobra.Command{Use: "daemon", Args: cobra.NoArgs, Short: "Run the frontend-independent DFS core",
		RunE: func(cmd *cobra.Command, args []string) error {
			signals := make(chan os.Signal, 2)
			signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(signals)
			logger, closer, err := newMountLogger(logLevel, logFormat, logFile, a.Err, false)
			if err != nil {
				return err
			}
			if closer != nil {
				defer closer.Close()
			}
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			if !managed {
				fmt.Fprintf(a.Out, "Running DFS core for %s; press Ctrl-C to stop\n", repo.Config.Repository)
			}
			return dfscore.Run(repo, dfscore.Options{Context: cmd.Context(), Logger: logger, Signals: signals,
				PairingPort: pairingPort, Mountpoint: mountpoint})
		}}
	cmd.Flags().StringVar(&logLevel, "log-level", "error", "logging level: debug, info, warn, or error")
	cmd.Flags().StringVar(&logFormat, "log-format", "text", "structured log format: text or json")
	cmd.Flags().StringVar(&logFile, "log-file", "", "append logs to this file as well as stderr")
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "", "configured frontend mountpoint for status display")
	cmd.Flags().IntVar(&pairingPort, "pair-port", peer.DefaultPairingPort, "UDP port for authenticated peer transport")
	cmd.Flags().BoolVar(&managed, "managed", false, "run under a service manager without interactive output")
	return cmd
}

func (a *App) unmountCommand() *cobra.Command {
	return &cobra.Command{
		Use: "unmount <mountpoint>", Aliases: []string{"umount"}, Args: cobra.ExactArgs(1), Short: "Unmount a DFS namespace",
		RunE: func(cmd *cobra.Command, args []string) error { return dfsmount.Unmount(args[0]) },
	}
}

func (a *App) healthCommand() *cobra.Command {
	return a.newHealthCommand("health", false)
}

func (a *App) optimizeCommand() *cobra.Command {
	var cluster, asJSON bool
	cmd := &cobra.Command{
		Use: "optimize", Args: cobra.NoArgs,
		Short: "Measure peer transfer performance and persist source priorities",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			progress := func(update managed.OptimizationProgress) {
				if asJSON {
					return
				}
				if update.Stage == "peer" {
					fmt.Fprintf(a.Out, "Optimizing %s...\n", optimizationPeerName(update.PeerName, update.PeerID))
					return
				}
				fmt.Fprintf(a.Out, "  %-16s %-11s sample %d/%d\n", optimizationPeerName(update.PeerName, update.PeerID), update.Stage, update.Sample, update.Samples)
			}
			if cluster {
				result, optimizeErr := managed.OptimizeCluster(ctx, repo, progress)
				if asJSON {
					return writeOptimizationJSON(a.Out, result)
				}
				printClusterOptimization(a.Out, result)
				return optimizeErr
			}
			state, optimizeErr := managed.OptimizeLocal(ctx, repo, progress)
			if asJSON {
				return writeOptimizationJSON(a.Out, state)
			}
			if optimizeErr == nil {
				printOptimizationState(a.Out, "Current peer", state)
			}
			return optimizeErr
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "optimize source priorities on every responding cluster peer")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the optimization result as JSON")
	return cmd
}

func writeOptimizationJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printClusterOptimization(output io.Writer, result managed.ClusterOptimization) {
	fmt.Fprintln(output, "\nDFS CLUSTER OPTIMIZATION")
	fmt.Fprintln(output, "\nDirected measurements")
	matrix := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(matrix, "FROM\tTO\tSTATUS\tTTFB MED/P95\tINTERACTIVE MED/P10\tBULK MED/P10")
	for _, outcome := range result.Peers {
		from := optimizationPeerName(outcome.PeerName, outcome.PeerID)
		if outcome.Error != "" {
			fmt.Fprintf(matrix, "%s\t-\tOFFLINE\t-\t-\t-\n", from)
			continue
		}
		for _, item := range outcome.State.Measurements {
			fmt.Fprintf(matrix, "%s\t%s\t%s\t%.1f/%.1f ms\t%.1f/%.1f Mbps\t%.1f/%.1f Mbps\n",
				from, optimizationPeerName(item.PeerName, item.PeerID), item.Status, item.TTFBMedianMS, item.TTFBP95MS,
				item.InteractiveMedianMbps, item.InteractiveP10Mbps, item.BulkMedianMbps, item.BulkP10Mbps)
		}
	}
	_ = matrix.Flush()
	for _, outcome := range result.Peers {
		name := optimizationPeerName(outcome.PeerName, outcome.PeerID)
		if outcome.Error != "" {
			fmt.Fprintf(output, "\n%s: OFFLINE (%s)\n", name, compactHealthDetail(outcome.Error))
			continue
		}
		printOptimizationState(output, name, outcome.State)
	}
}

func printOptimizationState(output io.Writer, heading string, state optimization.State) {
	status := "CURRENT"
	if state.Stale {
		status = "STALE"
	}
	fmt.Fprintf(output, "\n%s source priorities (%s, measured %s)\n", heading, status, formatHealthTime(state.OptimizedAt))
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "PROFILE\tORDER")
	fmt.Fprintf(table, "interactive\t%s\n", rankedSourceNames(state.Interactive))
	fmt.Fprintf(table, "bulk\t%s\n", rankedSourceNames(state.Bulk))
	_ = table.Flush()
	if len(state.Measurements) > 0 {
		measurements := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		fmt.Fprintln(measurements, "SOURCE\tSTATUS\tTTFB MED/P95\tINTERACTIVE MED/P10\tBULK MED/P10\tSAMPLES/FAILURES")
		for _, item := range state.Measurements {
			fmt.Fprintf(measurements, "%s\t%s\t%.1f/%.1f ms\t%.1f/%.1f Mbps\t%.1f/%.1f Mbps\t%d/%d\n",
				optimizationPeerName(item.PeerName, item.PeerID), item.Status, item.TTFBMedianMS, item.TTFBP95MS,
				item.InteractiveMedianMbps, item.InteractiveP10Mbps, item.BulkMedianMbps, item.BulkP10Mbps, item.Samples, item.Failures)
		}
		_ = measurements.Flush()
	}
}

func rankedSourceNames(sources []optimization.RankedSource) string {
	if len(sources) == 0 {
		return "not measured"
	}
	names := make([]string, 0, len(sources))
	for index, source := range sources {
		name := optimizationPeerName(source.PeerName, source.PeerID)
		if source.Status != "MEASURED" {
			name += " [" + strings.ToLower(source.Status) + "]"
		}
		names = append(names, fmt.Sprintf("%d. %s", index+1, name))
	}
	return strings.Join(names, "  ")
}

func optimizationPeerName(name, peerID string) string {
	if name != "" {
		return name
	}
	peerID = strings.TrimPrefix(peerID, "dfs-peer-")
	if len(peerID) > 12 {
		peerID = peerID[:12]
	}
	return peerID
}

type dependencyCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Path   string `json:"path,omitempty"`
}

type environmentHealth struct {
	Healthy bool              `json:"healthy"`
	Checks  []dependencyCheck `json:"checks"`
}

func (a *App) newHealthCommand(name string, deprecated bool) *cobra.Command {
	var asJSON, cluster bool
	var discoveryTimeout, peerTimeout time.Duration
	cmd := &cobra.Command{
		Use: name, Args: cobra.NoArgs, Short: "Report environment, filesystem, storage, and peer health",
		RunE: func(cmd *cobra.Command, args []string) error {
			environment, environmentErr := checkEnvironment(runtime.GOOS)
			repositoryPath, err := a.resolveRepository()
			if err != nil {
				return errors.Join(environmentErr, err)
			}
			report, healthErr := dfscore.CheckHealth(repositoryPath)
			if healthErr == nil && report.Operational != nil {
				if state, stateErr := optimization.LoadCurrent(repositoryPath, report.Operational.FileSystemID, report.Operational.PeerID); stateErr == nil {
					report.Operational.Optimization = &state
				}
			}
			var clusterReport *peer.MeshReport
			var clusterErr error
			if cluster && healthErr == nil {
				repo, openErr := a.open()
				if openErr != nil {
					clusterErr = openErr
				} else {
					defer repo.Close()
					meshReport, meshErr := peer.CheckMesh(cmd.Context(), repo, discoveryTimeout, peerTimeout)
					clusterReport = &meshReport
					clusterErr = meshErr
					if meshErr == nil && !meshReport.Complete {
						clusterErr = errors.New("DFS cluster health is degraded")
					}
				}
			}
			if asJSON {
				encoder := json.NewEncoder(a.Out)
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(struct {
					Environment environmentHealth    `json:"environment"`
					Service     dfscore.HealthReport `json:"service"`
					Cluster     *peer.MeshReport     `json:"cluster,omitempty"`
				}{Environment: environment, Service: report, Cluster: clusterReport}); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(a.Out, "DFS HEALTH")
				printEnvironmentHealth(a.Out, environment)
				if report.Version != 0 {
					printServiceDetails(a.Out, report)
				}
				if clusterReport != nil {
					printMeshHealth(a.Out, *clusterReport)
				}
			}
			return errors.Join(environmentErr, healthErr, clusterErr)
		},
	}
	if deprecated {
		cmd.Deprecated = "use dfs health instead"
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the complete health report as JSON")
	cmd.Flags().BoolVar(&cluster, "cluster", false, "actively check the entire cluster and every directed peer connection")
	cmd.Flags().DurationVar(&discoveryTimeout, "discovery-timeout", 2*time.Second, "how long to discover peers for the cluster check")
	cmd.Flags().DurationVar(&peerTimeout, "peer-timeout", 10*time.Second, "maximum time for each peer health probe")
	return cmd
}

func printServiceHealth(output io.Writer, report dfscore.HealthReport) {
	fmt.Fprintln(output, "DFS HEALTH")
	printServiceDetails(output, report)
}

func printServiceDetails(output io.Writer, report dfscore.HealthReport) {
	printServiceSummary(output, report)
	if report.Operational != nil {
		printNodeHealth(output, *report.Operational)
	} else if report.OperationalError != "" {
		fmt.Fprintf(output, "Operational check: ERROR (%s)\n", compactHealthDetail(report.OperationalError))
	} else {
		fmt.Fprintln(output, "Operational check: PENDING (periodic observation has not completed)")
	}
}

func printEnvironmentHealth(output io.Writer, report environmentHealth) {
	status := "HEALTHY"
	if !report.Healthy {
		status = "DEGRADED"
	}
	fmt.Fprintf(output, "Environment: %s\n", status)
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "DEPENDENCY\tSTATUS\tPATH")
	for _, check := range report.Checks {
		fmt.Fprintf(table, "%s\t%s\t%s\n", check.Name, check.Status, check.Path)
	}
	_ = table.Flush()
}

func printServiceSummary(output io.Writer, report dfscore.HealthReport) {
	fmt.Fprintf(output, "Core: %s  Peer: %s  PID: %d  Mount: %s  Updated: %s\n",
		strings.ToUpper(report.State), report.Peer, report.PID, report.Mountpoint, formatHealthTime(report.UpdatedAt))
}

func printNodeHealth(output io.Writer, report peer.DiagnosticReport) {
	status := nodeHealthStatus(report)
	id := report.FileSystemID
	if len(id) > 12 {
		id = id[:12]
	}
	fmt.Fprintf(output, "Filesystem: %s (%s)  Status: %s  Peer: %s (%s)  Port: %d\n",
		report.NetworkName, id, status, report.PeerName, report.Role, report.InstancePort)
	if report.ObservedAt.IsZero() {
		fmt.Fprintln(output, "Operational data unavailable: update this peer's DFS daemon.")
		return
	}
	fmt.Fprintf(output, "Namespace: %d files, %s logical  Reconciliation: %s  Members: %d (%d configured peers)\n",
		report.Stats.LogicalFiles, config.FormatSize(report.Stats.LogicalBytes), report.ReconciliationStatus,
		report.MembershipMembers, report.ConfiguredPeers)
	fmt.Fprintf(output, "Content: %d locally available paths, %s logical  Pins: %d (%d missing)\n",
		report.Stats.ContentFiles, config.FormatSize(report.Stats.ContentBytes), report.Stats.PinnedPaths, report.Stats.MissingPinnedFiles)
	fmt.Fprintf(output, "Storage: repo %s (metadata %s, private %s)  Cache: %s/%s (annex %s, ranges %s)  Disk free: %s/%s\n",
		config.FormatSize(report.Stats.RepositoryBytes), config.FormatSize(report.Stats.MetadataBytes),
		config.FormatSize(report.Stats.PrivateStateBytes), config.FormatSize(report.Stats.CacheBytes),
		config.FormatSize(report.Stats.CacheLimitBytes), config.FormatSize(report.Stats.AnnexCacheBytes),
		config.FormatSize(report.Stats.RangeCacheBytes), config.FormatSize(report.Stats.DiskAvailableBytes),
		config.FormatSize(report.Stats.DiskTotalBytes))
	printPinnedHealth(output, "", report.Stats.Pinned)
	if report.Optimization != nil {
		printOptimizationState(output, "Current peer", *report.Optimization)
	}
	if len(report.Remotes) > 0 {
		fmt.Fprintln(output, "\nPeers")
		table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "PEER\tSTATUS\tTRANSPORT\tDETAIL")
		for _, remote := range report.Remotes {
			remoteStatus, detail := "OK", ""
			if !remote.Reachable {
				remoteStatus, detail = "UNREACHABLE", compactHealthDetail(remote.Error)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", remoteHealthName(remote), remoteStatus, remote.Transport, detail)
		}
		_ = table.Flush()
	}
	printHealthIssues(output, report.Issues, nil)
}

func printMeshHealth(output io.Writer, report peer.MeshReport) {
	clusterStatus := "HEALTHY"
	if !report.Complete {
		clusterStatus = "DEGRADED"
	}
	fmt.Fprintln(output, "\nDFS CLUSTER HEALTH")
	fmt.Fprintf(output, "Status: %s  Namespace: %s  Responding: %d/%d  Observed: %s\n",
		clusterStatus, strings.ToUpper(report.NamespaceStatus), len(report.Reports), len(report.Peers), formatHealthTime(report.ObservedAt))

	reports := make(map[string]peer.DiagnosticReport, len(report.Reports))
	for _, node := range report.Reports {
		reports[node.PeerID] = node
	}
	fmt.Fprintln(output, "\nPeers")
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "PEER\tROLE\tSTATUS\tPORT\tFILES\tLOGICAL\tCONTENT\tCACHE\tDISK FREE\tRECONCILIATION\tOBSERVED")
	for _, participant := range report.Peers {
		node, found := reports[participant.PeerID]
		if !found {
			fmt.Fprintf(table, "%s\t-\tUNREPORTED\t-\t-\t-\t-\t-\t-\t-\t-\n", meshPeerLabel(participant))
			continue
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s/%s\t%s\t%s\t%s\n",
			meshPeerLabel(participant), node.Role, nodeHealthStatus(node), node.InstancePort,
			node.Stats.LogicalFiles, config.FormatSize(node.Stats.LogicalBytes), config.FormatSize(node.Stats.ContentBytes),
			config.FormatSize(node.Stats.CacheBytes), config.FormatSize(node.Stats.CacheLimitBytes),
			config.FormatSize(node.Stats.DiskAvailableBytes), node.ReconciliationStatus, formatHealthTime(node.ObservedAt))
	}
	_ = table.Flush()
	var optimized bool
	for _, participant := range report.Peers {
		if node, found := reports[participant.PeerID]; found && node.Optimization != nil {
			if !optimized {
				fmt.Fprintln(output, "\nSource priorities")
				optimized = true
			}
			printOptimizationState(output, meshPeerLabel(participant), *node.Optimization)
		}
	}
	var pinnedRows int
	for _, node := range report.Reports {
		pinnedRows += len(node.Stats.Pinned)
	}
	if pinnedRows > 0 {
		fmt.Fprintln(output, "\nPinned content")
		pins := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		fmt.Fprintln(pins, "PEER\tPATH\tSCOPE\tSTATUS\tTYPE\tFILES\tLOGICAL\tMISSING")
		for _, participant := range report.Peers {
			node, found := reports[participant.PeerID]
			if !found {
				continue
			}
			for _, pin := range node.Stats.Pinned {
				fmt.Fprintf(pins, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\n", meshPeerLabel(participant), displayPinnedPath(pin.Path),
					strings.ToUpper(pin.Scope), strings.ToUpper(pin.Status), strings.ToUpper(pin.Kind), pin.LogicalFiles, config.FormatSize(pin.LogicalBytes), pin.MissingFiles)
			}
		}
		_ = pins.Flush()
	}
	printMeshReport(output, report)

	issues := append([]peer.HealthIssue(nil), report.Issues...)
	for _, node := range report.Reports {
		for _, issue := range node.Issues {
			switch issue.Code {
			case "PEER_UNREACHABLE":
				continue // The directed connection table communicates these once and more clearly.
			}
			issues = append(issues, issue)
		}
	}
	printHealthIssues(output, issues, nil)
}

func printPinnedHealth(output io.Writer, peerName string, pinned []repository.PinnedPathHealth) {
	if len(pinned) == 0 {
		return
	}
	fmt.Fprintln(output, "\nPinned content")
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if peerName == "" {
		fmt.Fprintln(table, "PATH\tSCOPE\tSTATUS\tTYPE\tFILES\tLOGICAL\tMISSING")
	} else {
		fmt.Fprintln(table, "PEER\tPATH\tSCOPE\tSTATUS\tTYPE\tFILES\tLOGICAL\tMISSING")
	}
	for _, pin := range pinned {
		if peerName != "" {
			fmt.Fprintf(table, "%s\t", peerName)
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%s\t%d\n", displayPinnedPath(pin.Path), strings.ToUpper(pin.Scope),
			strings.ToUpper(pin.Status), strings.ToUpper(pin.Kind), pin.LogicalFiles, config.FormatSize(pin.LogicalBytes), pin.MissingFiles)
	}
	_ = table.Flush()
}

func displayPinnedPath(path string) string {
	if path == "" {
		return "."
	}
	return path
}

func nodeHealthStatus(report peer.DiagnosticReport) string {
	if report.ObservedAt.IsZero() {
		return "UNKNOWN"
	}
	status := "OK"
	for _, issue := range report.Issues {
		if issue.Severity == "error" {
			return "ERROR"
		}
		status = "DEGRADED"
	}
	return status
}

func remoteHealthName(remote peer.RemoteDiagnostic) string {
	if remote.PeerName != "" {
		return remote.PeerName
	}
	name := strings.TrimPrefix(remote.Name, "dfs-peer-")
	if len(name) > 12 {
		name = name[:12]
	}
	return name
}

func formatHealthTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func compactHealthDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	lower := strings.ToLower(detail)
	for _, summary := range []struct{ match, text string }{
		{"connection timed out", "connection timed out"},
		{"context deadline exceeded", "connection timed out"},
		{"connection refused", "connection refused"},
		{"no route to host", "no route to host"},
		{"permission denied", "permission denied"},
		{"host key verification failed", "host key verification failed"},
	} {
		if strings.Contains(lower, summary.match) {
			return summary.text
		}
	}
	if len(detail) > 100 {
		return detail[:97] + "..."
	}
	return detail
}

func printHealthIssues(output io.Writer, issues []peer.HealthIssue, skip map[string]bool) {
	seen := make(map[string]bool)
	printedHeader := false
	for _, issue := range issues {
		if skip != nil && skip[issue.Code] {
			continue
		}
		key := issue.Code + "|" + compactHealthDetail(issue.Detail)
		if seen[key] {
			continue
		}
		seen[key] = true
		if !printedHeader {
			fmt.Fprintln(output, "\nIssues")
			printedHeader = true
		}
		fmt.Fprintf(output, "%s %s: %s\n  Action: %s\n", strings.ToUpper(issue.Severity), issue.Code,
			compactHealthDetail(issue.Detail), issue.Action)
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
	var cluster bool
	cmd := &cobra.Command{
		Use: "pin <path>...", Args: cobra.MinimumNArgs(1), Short: "Hydrate content automatically and protect it from eviction",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			for _, path := range args {
				var err error
				if cluster {
					err = repo.SetClusterPin(ctx, path, true)
				} else {
					err = repo.SetLocalPin(path)
				}
				if err != nil {
					return err
				}
			}
			if err := wakeup.Notify(repo.Config.Repository, "pin policy changed"); err == nil {
				fmt.Fprintf(a.Out, "Pin policy saved; background hydration scheduled for %d path(s)\n", len(args))
				return nil
			}
			if cluster {
				if err := peer.ReconcileMembership(ctx, repo); err != nil {
					return err
				}
			}
			for _, path := range args {
				if err := repo.Fetch(ctx, path, ""); err != nil {
					return fmt.Errorf("pin policy saved; hydration failed: %w", err)
				}
			}
			fmt.Fprintf(a.Out, "Pinned and hydrated %d path(s)\n", len(args))
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "pin on every current and future cluster peer")
	return cmd
}

func (a *App) unpinCommand() *cobra.Command {
	var cluster bool
	cmd := &cobra.Command{
		Use: "unpin <path>...", Args: cobra.MinimumNArgs(1), Short: "Allow content to be evicted",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := a.open()
			if err != nil {
				return err
			}
			defer repo.Close()
			ctx, cancel := commandContext(cmd)
			defer cancel()
			for _, path := range args {
				var err error
				if cluster {
					err = repo.SetClusterPin(ctx, path, false)
				} else {
					err = repo.Unpin(path)
				}
				if err != nil {
					return err
				}
			}
			if err := wakeup.Notify(repo.Config.Repository, "pin policy changed"); err != nil && cluster {
				if err := peer.ReconcileMembership(ctx, repo); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "remove the replicated cluster-wide pin policy")
	return cmd
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
				pins, err := repo.Store.PinRecords()
				if err != nil {
					return err
				}
				fmt.Fprintf(a.Out, "%s / %s used\n", config.FormatSize(usage), config.FormatSize(repo.Config.CacheLimit))
				for _, pin := range pins {
					fmt.Fprintf(a.Out, "pinned\t%s\t%s\n", pin.Scope, displayPinnedPath(pin.Path))
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
	return a.newHealthCommand("doctor", true)
}

func checkEnvironment(goos string) (environmentHealth, error) {
	report := environmentHealth{Healthy: true}
	if err := prepareDoctorPath(goos); err != nil {
		return report, fmt.Errorf("prepare dependency search path: %w", err)
	}
	commands := []string{"git", "git-annex"}
	if goos == "linux" {
		commands = append(commands, "fusermount3")
	}
	for _, name := range commands {
		path, err := exec.LookPath(name)
		check := dependencyCheck{Name: name, Status: "OK", Path: path}
		if err != nil {
			report.Healthy = false
			check.Status = "MISSING"
		}
		report.Checks = append(report.Checks, check)
	}
	if goos == "linux" {
		check := dependencyCheck{Name: "/dev/fuse", Status: "OK", Path: "/dev/fuse"}
		if _, err := os.Stat("/dev/fuse"); err != nil {
			report.Healthy = false
			check.Status = "MISSING"
			check.Path = ""
		}
		report.Checks = append(report.Checks, check)
	}
	if goos == "darwin" {
		check := dependencyCheck{Name: "macFUSE", Status: "MISSING"}
		for _, path := range []string{
			"/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse",
			"/Library/Filesystems/osxfuse.fs/Contents/Resources/mount_osxfuse",
		} {
			if _, err := os.Stat(path); err == nil {
				check.Status = "OK"
				check.Path = path
				break
			}
		}
		if check.Status != "OK" {
			report.Healthy = false
		}
		report.Checks = append(report.Checks, check)
	}
	if !report.Healthy {
		return report, errors.New("one or more required dependencies are missing")
	}
	return report, nil
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
		fmt.Fprintf(output, "\nConnections\n%s is the only cluster peer.\n", meshPeerLabel(report.Peers[0]))
		return
	}
	names := make(map[string]string, len(report.Peers))
	for _, participant := range report.Peers {
		names[participant.PeerID] = meshPeerLabel(participant)
	}
	fmt.Fprintln(output, "\nConnections")
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "FROM\tTO\tSTATUS\tDETAIL")
	for _, connection := range report.Connections {
		detail := compactHealthDetail(connection.Error)
		if detail == "" && connection.Status == "OK" {
			detail = "QUIC"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n",
			names[connection.FromPeerID], names[connection.ToPeerID], connection.Status, detail)
	}
	_ = table.Flush()
}

func meshPeerLabel(participant peer.MeshPeer) string {
	id := participant.PeerID
	if len(id) > 12 {
		id = id[:12]
	}
	if participant.PeerName == "" || participant.PeerName == participant.PeerID || strings.HasPrefix(participant.PeerName, "dfs-peer-") {
		return id
	}
	return participant.PeerName
}
