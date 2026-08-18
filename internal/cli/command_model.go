package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	dfsinstance "github.com/bitbeamer/dfs/internal/instance"
	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/membership"
	dfspeer "github.com/bitbeamer/dfs/internal/peer"
	"github.com/spf13/cobra"
)

func (a *App) publicCommands() []*cobra.Command {
	return []*cobra.Command{
		a.setupGroupCommand(),
		a.filesystemCommand(),
		a.serviceCommand(),
		a.upgradeCommand(),
		a.consolidatedPeerCommand(),
		a.contentCommand(),
		a.consolidatedCacheCommand(),
		a.consolidatedStorageCommand(),
		a.consolidatedSyncCommand(),
		a.scopedHealthCommand(),
		a.consolidatedHistoryCommand(),
	}
}

func (a *App) setupGroupCommand() *cobra.Command {
	root := a.setupModeCommand("join")
	root.Use = "setup"
	root.Short = "Create or join, install, mount, and verify DFS"
	root.AddCommand(
		a.setupModeCommand("create"),
		a.setupModeCommand("join"),
		a.setupModeCommand("resume"),
		a.setupModeCommand("abort"),
	)
	join := root.RunE
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if a.output == "json" {
			return errors.New("dfs setup with JSON output requires an explicit create, join, resume, or abort subcommand")
		}
		fmt.Fprint(a.Err, "Create or join a DFS filesystem? [create/join]: ")
		answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && len(answer) == 0 {
			return errors.New("interactive setup requires a choice; use dfs setup create or dfs setup join")
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "create", "c":
			if err := root.Flags().Set("create", "true"); err != nil {
				return err
			}
		case "join", "j":
		default:
			return errors.New("choose create or join")
		}
		return join(cmd, args)
	}
	a.addDryRun(root, "setup")
	for _, child := range root.Commands() {
		if child.Name() == "abort" {
			markConfirmed(child)
		}
		a.addDryRun(child, "setup "+child.Name())
	}
	return root
}

func (a *App) setupModeCommand(mode string) *cobra.Command {
	command := a.setupCommand()
	command.Use = mode
	command.Args = cobra.NoArgs
	for _, name := range []string{"create", "resume", "abort"} {
		_ = command.Flags().MarkHidden(name)
	}
	if mode != "join" {
		_ = command.Flags().MarkHidden("filesystem")
	}
	switch mode {
	case "create":
		_ = command.Flags().Set("create", "true")
		command.Short = "Create and install the first peer of a new filesystem"
	case "join":
		command.Short = "Discover, join, install, and verify a filesystem"
	case "resume":
		_ = command.Flags().Set("resume", "true")
		command.Short = "Resume an interrupted setup transaction"
	case "abort":
		_ = command.Flags().Set("abort", "true")
		command.Short = "Roll back an interrupted setup transaction"
	}
	aliasFlag(command, "name", "peer-name", "peer display name (defaults to hostname)")
	aliasFlag(command, "network-name", "filesystem-name", "filesystem display name")
	aliasFlag(command, "pair-port", "transport-port", "authenticated QUIC transport port")
	aliasFlag(command, "git-name", "author-name", "history author name")
	aliasFlag(command, "git-email", "author-email", "history author email")
	aliasFlag(command, "repository", "data-dir", "private DFS data directory")
	aliasFlag(command, "timeout", "discovery-timeout", "LAN discovery window")
	for _, legacy := range []string{"name", "network-name", "pair-port", "git-name", "git-email", "repository", "timeout"} {
		_ = command.Flags().MarkHidden(legacy)
	}
	return command
}

func aliasFlag(command *cobra.Command, existing, replacement, usage string) {
	flag := command.Flags().Lookup(existing)
	if flag == nil || command.Flags().Lookup(replacement) != nil {
		return
	}
	command.Flags().Var(flag.Value, replacement, usage)
}

func (a *App) filesystemCommand() *cobra.Command {
	command := &cobra.Command{Use: "filesystem", Short: "Inspect the selected DFS filesystem"}
	command.AddCommand(&cobra.Command{Use: "show", Args: cobra.NoArgs, Short: "Show filesystem identity and local configuration", RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		if a.output == "json" {
			return writeJSON(a.Out, map[string]any{"filesystem_id": repo.Config.FileSystemID, "name": repo.Config.NetworkName, "peer_id": repo.Config.PeerID,
				"peer_name": repo.Config.Name, "repository": repo.Config.Repository, "cache_limit_bytes": repo.Config.CacheLimit})
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Filesystem: %s\nID: %s\nPeer: %s (%s)\nRepository: %s\n", repo.Config.NetworkName, repo.Config.FileSystemID, repo.Config.Name, repo.Config.PeerID, repo.Config.Repository)
		}
		return nil
	}})
	command.AddCommand(a.filesystemRenameCommand())
	return command
}

func (a *App) filesystemRenameCommand() *cobra.Command {
	var dryRun bool
	command := &cobra.Command{Use: "rename <name>", Args: cobra.ExactArgs(1), Short: "Publish a signed canonical filesystem display name", RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		filesystemID, err := repo.FileSystemID(cmd.Context())
		if err != nil {
			return err
		}
		if dryRun {
			if a.output == "json" {
				return writeJSON(a.Out, map[string]any{"action": "filesystem rename", "dry_run": true, "filesystem_id": filesystemID, "old_name": repo.Config.NetworkName, "new_name": args[0]})
			}
			if !a.quiet {
				fmt.Fprintf(a.Out, "Dry run: rename filesystem %q to %q across the cluster\n", repo.Config.NetworkName, args[0])
			}
			return nil
		}
		if err := a.confirm(cmd, "rename filesystem across the cluster", 1); err != nil {
			return err
		}
		value, err := membership.SetFilesystemName(repo.Config.Repository, filesystemID, repo.Config.PeerID, args[0])
		if err != nil {
			return err
		}
		if err := repo.SetNetworkName(value.Name); err != nil {
			return fmt.Errorf("signed filesystem name was saved but local configuration failed: %w", err)
		}
		if err := dfspeer.ReconcileMembership(cmd.Context(), repo); err != nil {
			return fmt.Errorf("signed filesystem name was saved locally but cluster reconciliation failed: %w", err)
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Renamed filesystem to %q with signed generation %d\n", value.Name, value.Generation)
		}
		return nil
	}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "preview the signed cluster-wide rename")
	return command
}

func (a *App) serviceCommand() *cobra.Command {
	command := a.instanceCommand()
	command.Use = "service"
	command.Short = "Manage local DFS system services"
	for _, child := range command.Commands() {
		switch child.Name() {
		case "update", "stop", "uninstall":
			command.RemoveCommand(child)
		case "list":
			run := child.RunE
			child.RunE = func(cmd *cobra.Command, args []string) error {
				if a.hasFilesystemSelector() {
					return errors.New("service list is host-scoped and does not accept --filesystem")
				}
				return run(cmd, args)
			}
			_ = child.Flags().MarkHidden("json")
			child.Short = "List local DFS services and their filesystems"
		}
	}
	command.AddCommand(a.serviceStateCommand("start"), a.serviceStateCommand("stop"), a.serviceStateCommand("restart"), a.serviceRepairCommand(), a.serviceUninstallCommand())
	command.AddCommand(a.serviceShowCommand())
	return command
}

func (a *App) serviceShowCommand() *cobra.Command {
	return &cobra.Command{Use: "show [name|id|mountpoint|repository]", Args: cobra.MaximumNArgs(1), Short: "Show one local DFS service", RunE: func(cmd *cobra.Command, args []string) error {
		instances, err := dfsinstance.Discover(cmd.Context())
		if err != nil {
			return err
		}
		selected, err := selectManagedInstances(instances, args, false, a.filesystem)
		if err != nil {
			return err
		}
		instance := selected[0]
		if a.output == "json" {
			return writeJSON(a.Out, instance)
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Filesystem: %s (%s)\nPeer: %s\nStatus: %s\nEnabled: core=%t mount=%t\nBinary: %s\nMountpoint: %s\nRepository: %s\nTransport port: %d\n",
				instance.NetworkName, shortID(instance.FileSystemID), instance.Name, instanceStatus(instance), instance.CoreEnabled, instance.MountEnabled,
				instance.Binary, instance.Mountpoint, instance.Repository, instance.PairingPort)
		}
		return nil
	}}
}

func (a *App) serviceStateCommand(action string) *cobra.Command {
	var all, dryRun bool
	command := &cobra.Command{Use: action + " [name|id|mountpoint|repository]", Args: cobra.MaximumNArgs(1), Short: strings.Title(action) + " one or every local DFS service",
		RunE: func(cmd *cobra.Command, args []string) error {
			instances, err := dfsinstance.Discover(cmd.Context())
			if err != nil {
				return err
			}
			selected, err := selectManagedInstances(instances, args, all, a.filesystem)
			if err != nil {
				return err
			}
			if dryRun {
				return a.writeMutationPlan("service "+action, selected, nil)
			}
			if all {
				if err := a.confirm(cmd, "service "+action, len(selected)); err != nil {
					return err
				}
			}
			if action == "start" {
				err = dfsinstance.Start(cmd.Context(), selected)
			} else if action == "stop" {
				err = dfsinstance.Stop(cmd.Context(), selected)
			} else {
				err = dfsinstance.Restart(cmd.Context(), selected)
			}
			if err == nil && !a.quiet {
				pastTense := map[string]string{"start": "Started", "stop": "Stopped", "restart": "Restarted"}[action]
				fmt.Fprintf(a.Out, "%s %d DFS service(s)\n", pastTense, len(selected))
			}
			return err
		}}
	command.Flags().BoolVar(&all, "all", false, "target every local DFS service")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "show the service plan without changing services")
	return command
}

func (a *App) serviceUninstallCommand() *cobra.Command {
	var all, dryRun bool
	command := &cobra.Command{Use: "uninstall [name|id|mountpoint|repository]", Args: cobra.MaximumNArgs(1), Short: "Remove local service definitions while retaining repository data", RunE: func(cmd *cobra.Command, args []string) error {
		instances, err := dfsinstance.Discover(cmd.Context())
		if err != nil {
			return err
		}
		selected, err := selectManagedInstances(instances, args, all, a.filesystem)
		if err != nil {
			return err
		}
		if dryRun {
			return a.writeMutationPlan("service uninstall (repository data retained)", selected, nil)
		}
		if all {
			if err := a.confirm(cmd, "service uninstall with repository data retained", len(selected)); err != nil {
				return err
			}
		}
		if err := dfsinstance.Uninstall(cmd.Context(), selected); err != nil {
			return err
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Uninstalled %d DFS service(s); repository data and peer membership were retained\n", len(selected))
			for _, instance := range selected {
				fmt.Fprintf(a.Out, "  Retained: %s\n", instance.Repository)
			}
		}
		return nil
	}}
	command.Flags().BoolVar(&all, "all", false, "uninstall every local DFS service")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "show the uninstall plan without changing services")
	return command
}

func (a *App) confirm(command *cobra.Command, action string, targets int) error {
	if a.yes {
		return nil
	}
	if a.output == "json" {
		return fmt.Errorf("%s affecting %d target(s) requires --yes with JSON output", action, targets)
	}
	fmt.Fprintf(a.Err, "%s will affect %d target(s). Continue? [y/N] ", strings.Title(action), targets)
	answer, err := bufio.NewReader(command.InOrStdin()).ReadString('\n')
	if err != nil && len(answer) == 0 {
		return err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	if answer != "y" && answer != "yes" {
		return errors.New("operation was not approved")
	}
	return nil
}

func (a *App) upgradeCommand() *cobra.Command {
	var candidate string
	var dryRun bool
	command := &cobra.Command{Use: "upgrade", Args: cobra.NoArgs, Short: "Replace the shared DFS executable and safely update local services", RunE: func(cmd *cobra.Command, _ []string) error {
		if a.hasFilesystemSelector() {
			return errors.New("upgrade is host-scoped and does not accept --filesystem")
		}
		if strings.TrimSpace(candidate) == "" {
			return errors.New("--from is required for a source-built DFS upgrade")
		}
		instances, err := dfsinstance.Discover(cmd.Context())
		if err != nil {
			return err
		}
		if len(instances) == 0 {
			return errors.New("no managed DFS services found")
		}
		if dryRun {
			if err := dfsinstance.ValidateUpgrade(cmd.Context(), instances, candidate, ""); err != nil {
				return err
			}
			return a.writeMutationPlan("upgrade", instances, map[string]any{"candidate": candidate})
		}
		if err := a.confirm(cmd, "upgrade", len(instances)); err != nil {
			return err
		}
		if err := dfsinstance.Update(cmd.Context(), instances, candidate, ""); err != nil {
			return err
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Upgraded %d DFS service(s) from %s\n", len(instances), candidate)
		}
		return nil
	}}
	command.Flags().StringVar(&candidate, "from", "", "candidate DFS executable")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "show the upgrade plan without changing services")
	return command
}

func (a *App) hasFilesystemSelector() bool {
	return strings.TrimSpace(a.filesystem) != "" || strings.TrimSpace(os.Getenv("DFS_FILESYSTEM")) != ""
}

func (a *App) serviceRepairCommand() *cobra.Command {
	var all, dryRun bool
	command := &cobra.Command{Use: "repair [name|id|mountpoint|repository]", Args: cobra.MaximumNArgs(1), Short: "Validate and reinstall service definitions without changing software", RunE: func(cmd *cobra.Command, args []string) error {
		instances, err := dfsinstance.Discover(cmd.Context())
		if err != nil {
			return err
		}
		selected, err := selectManagedInstances(instances, args, all, a.filesystem)
		if err != nil {
			return err
		}
		if dryRun {
			return a.writeMutationPlan("service repair", selected, nil)
		}
		if all {
			if err := a.confirm(cmd, "service repair", len(selected)); err != nil {
				return err
			}
		}
		binary := selected[0].Binary
		if binary == "" {
			return errors.New("selected DFS service does not identify its installed executable")
		}
		for _, instance := range selected[1:] {
			if instance.Binary != binary {
				return errors.New("selected DFS services do not share one installed executable")
			}
		}
		if err := dfsinstance.Repair(cmd.Context(), selected, binary, ""); err != nil {
			return err
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Repaired %d DFS service(s) without changing the executable\n", len(selected))
		}
		return nil
	}}
	command.Flags().BoolVar(&all, "all", false, "repair every local DFS service")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "show the repair plan without changing services")
	return command
}

func (a *App) writeMutationPlan(action string, instances []dfsinstance.Instance, details map[string]any) error {
	if a.output == "json" {
		return writeJSON(a.Out, map[string]any{"action": action, "dry_run": true, "targets": instances, "details": details})
	}
	if a.quiet {
		return nil
	}
	fmt.Fprintf(a.Out, "Dry run: %s would affect %d DFS service(s)\n", action, len(instances))
	for _, instance := range instances {
		fmt.Fprintf(a.Out, "  %s (%s) at %s\n", instance.NetworkName, shortID(instance.FileSystemID), instance.Mountpoint)
	}
	return nil
}

func (a *App) consolidatedPeerCommand() *cobra.Command {
	command := &cobra.Command{Use: "peer", Short: "Manage membership, admission, and peer connectivity"}
	pair := a.pairCommand()
	relay := a.relayCommand()

	admission := detachChildren(pair, "requests", "approve", "reject")
	for _, child := range admission {
		if child.Name() != "requests" {
			markConfirmed(child)
			a.addDryRun(child, "peer "+child.Name())
		}
	}
	command.AddCommand(admission...)
	invite := &cobra.Command{Use: "invite", Short: "Manage optional out-of-band invitations"}
	for _, child := range detachChildren(pair, "invite", "list", "revoke") {
		if child.Name() == "invite" {
			child.Use = "create"
		}
		if child.Name() != "list" {
			if child.Name() == "revoke" {
				markConfirmed(child)
			}
			a.addDryRun(child, "peer invite "+child.Name())
		}
		invite.AddCommand(child)
	}
	command.AddCommand(invite)
	command.AddCommand(a.peerListCommand())
	command.AddCommand(a.peerRemoveCommand())
	command.AddCommand(a.peerCheckCommand())
	optimize := a.optimizeCommand()
	optimize.Use = "optimize"
	_ = optimize.Flags().MarkHidden("json")
	addScopeFlag(optimize)
	a.addDryRun(optimize, "peer optimize")
	command.AddCommand(optimize)
	relay.Use = "relay"
	for _, child := range relay.Commands() {
		if child.Name() == "status" {
			child.Use = "show"
		} else {
			a.addDryRun(child, "peer relay "+child.Name())
		}
	}
	clear := &cobra.Command{Use: "clear", Args: cobra.NoArgs, Short: "Remove the optional metadata relay configuration", RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		return repo.ClearRelay(cmd.Context())
	}}
	a.addDryRun(clear, "peer relay clear")
	relay.AddCommand(clear)
	command.AddCommand(relay)
	return command
}

func (a *App) peerCheckCommand() *cobra.Command {
	var scope string
	var discoveryTimeout, peerTimeout time.Duration
	command := &cobra.Command{Use: "check [peer-id]", Args: cobra.MaximumNArgs(1), Short: "Check one peer path or every directed cluster path", RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		switch scope {
		case "local":
			if len(args) != 1 {
				return errors.New("peer check --scope local requires one peer ID")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), peerTimeout)
			defer cancel()
			if err := managed.Probe(ctx, repo, args[0]); err != nil {
				return err
			}
			if a.output == "json" {
				return writeJSON(a.Out, map[string]any{"peer_id": args[0], "status": "reachable", "authenticated": true})
			}
			if !a.quiet {
				fmt.Fprintln(a.Out, "Peer path is reachable and mutually authenticated")
			}
			return nil
		case "cluster":
			if len(args) != 0 {
				return errors.New("peer check --scope cluster does not accept one peer")
			}
			report, checkErr := dfspeer.CheckMesh(cmd.Context(), repo, discoveryTimeout, peerTimeout)
			if a.output == "json" {
				if err := writeJSON(a.Out, report); err != nil {
					return err
				}
			} else if !a.quiet {
				printMeshHealth(a.Out, report)
			}
			if checkErr != nil {
				return checkErr
			}
			if !report.Complete {
				return errors.New("DFS cluster peer paths are degraded")
			}
			return nil
		default:
			return fmt.Errorf("unsupported scope %q; use local or cluster", scope)
		}
	}}
	command.Flags().StringVar(&scope, "scope", "local", "check scope: local or cluster")
	command.Flags().DurationVar(&discoveryTimeout, "discovery-timeout", 2*time.Second, "LAN discovery window for a cluster check")
	command.Flags().DurationVar(&peerTimeout, "peer-timeout", 10*time.Second, "maximum time for each authenticated peer check")
	return command
}

func (a *App) peerRemoveCommand() *cobra.Command {
	var dryRun bool
	command := &cobra.Command{Use: "remove <peer>", Args: cobra.ExactArgs(1), Short: "Revoke an accepted peer and remove its local transport", RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		filesystemID, err := repo.FileSystemID(cmd.Context())
		if err != nil {
			return err
		}
		records, err := membership.Accepted(repo.Config.Repository, filesystemID, repo.Config.PeerID)
		if err != nil {
			return err
		}
		selector := strings.TrimPrefix(strings.TrimSpace(args[0]), "dfs-peer-")
		var matches []membership.Record
		for _, record := range records {
			if record.Payload.PeerID == repo.Config.PeerID {
				continue
			}
			if record.Payload.Name == args[0] || record.Payload.PeerID == selector || strings.HasPrefix(record.Payload.PeerID, selector) {
				matches = append(matches, record)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("peer selector %q matches %d accepted peers", args[0], len(matches))
		}
		target := matches[0].Payload
		if dryRun {
			if a.output == "json" {
				return writeJSON(a.Out, map[string]any{"action": "peer remove", "dry_run": true, "peer": target})
			}
			if !a.quiet {
				fmt.Fprintf(a.Out, "Dry run: revoke %s (%s); previously copied content cannot be erased\n", target.Name, shortID(target.PeerID))
			}
			return nil
		}
		if err := a.confirm(cmd, "revoke peer "+target.Name, 1); err != nil {
			return err
		}
		remote := "dfs-peer-" + shortID(target.PeerID)
		if err := dfspeer.RevokeMembership(cmd.Context(), repo, remote); err != nil {
			return err
		}
		if err := repo.RemovePeer(cmd.Context(), remote); err != nil {
			return fmt.Errorf("membership revoked but local transport removal failed: %w", err)
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Revoked peer %s (%s); content already copied to that peer cannot be erased\n", target.Name, shortID(target.PeerID))
		}
		return nil
	}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "preview membership and transport removal")
	return command
}

func (a *App) peerListCommand() *cobra.Command {
	return &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List accepted signed filesystem members", RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		filesystemID, err := repo.FileSystemID(cmd.Context())
		if err != nil {
			return err
		}
		records, err := membership.Accepted(repo.Config.Repository, filesystemID, repo.Config.PeerID)
		if err != nil {
			return err
		}
		if a.output == "json" {
			members := make([]map[string]any, 0, len(records))
			for _, record := range records {
				payload := record.Payload
				members = append(members, map[string]any{"name": payload.Name, "peer_id": payload.PeerID, "role": payload.Role,
					"hostname": payload.Hostname, "endpoint": payload.QUICEndpoint, "current": payload.PeerID == repo.Config.PeerID})
			}
			return writeJSON(a.Out, map[string]any{"filesystem_id": filesystemID, "members": members})
		}
		if a.quiet {
			return nil
		}
		writer := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "PEER\tID\tROLE\tHOSTNAME\tENDPOINT")
		for _, record := range records {
			payload := record.Payload
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", payload.Name, shortID(payload.PeerID), payload.Role, payload.Hostname, payload.QUICEndpoint)
		}
		return writer.Flush()
	}}
}

func detachChildren(parent *cobra.Command, names ...string) []*cobra.Command {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	var result []*cobra.Command
	for _, child := range parent.Commands() {
		if wanted[child.Name()] {
			parent.RemoveCommand(child)
			result = append(result, child)
		}
	}
	return result
}

func (a *App) contentCommand() *cobra.Command {
	command := &cobra.Command{Use: "content", Short: "Manage content residency and pin policy"}
	for _, child := range []*cobra.Command{a.fetchCommand(), a.pinCommand(), a.unpinCommand(), a.evictCommand()} {
		if child.Name() == "fetch" {
			aliasFlag(child, "from", "source", "preferred peer or durable storage source")
			_ = child.Flags().MarkHidden("from")
		}
		if child.Flags().Lookup("cluster") != nil {
			addScopeFlag(child)
		}
		if child.Name() == "evict" {
			markConfirmed(child)
		}
		a.addDryRun(child, "content "+child.Name())
		command.AddCommand(child)
	}
	return command
}

func (a *App) consolidatedCacheCommand() *cobra.Command {
	legacy := a.cacheCommand()
	command := &cobra.Command{Use: "cache", Short: legacy.Short}
	for _, child := range legacy.Commands() {
		switch child.Name() {
		case "status":
			child.Use = "show"
		case "set-limit":
			child.Use = strings.Replace(child.Use, "set-limit", "limit", 1)
		}
		if child.Name() != "show" {
			if child.Name() == "prune" {
				markConfirmed(child)
			}
			a.addDryRun(child, "cache "+child.Name())
		}
		legacy.RemoveCommand(child)
		command.AddCommand(child)
	}
	return command
}

func (a *App) consolidatedStorageCommand() *cobra.Command {
	command := a.storageCommand()
	for _, child := range command.Commands() {
		if child.Name() == "add-s3" {
			command.RemoveCommand(child)
			child.Use = strings.Replace(child.Use, "add-s3", "s3", 1)
			add := &cobra.Command{Use: "add", Short: "Add durable storage"}
			add.AddCommand(child)
			command.AddCommand(add)
			a.addDryRun(child, "storage add s3")
		} else {
			a.addDryRun(child, "storage "+child.Name())
		}
	}
	command.AddCommand(a.storageListCommand(), a.storageShowCommand(), a.storageRemoveCommand())
	return command
}

func (a *App) storageListCommand() *cobra.Command {
	return &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List configured durable storage", RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		storages, err := repo.Storages(cmd.Context())
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(a.Out, map[string]any{"storage": storages})
		}
		if !a.quiet {
			for _, storage := range storages {
				fmt.Fprintf(a.Out, "%s\t%s\n", storage.Name, storage.UUID)
			}
		}
		return nil
	}}
}

func (a *App) storageShowCommand() *cobra.Command {
	return &cobra.Command{Use: "show <name>", Args: cobra.ExactArgs(1), Short: "Show one durable storage configuration", RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		storages, err := repo.Storages(cmd.Context())
		if err != nil {
			return err
		}
		for _, storage := range storages {
			if storage.Name == args[0] {
				if a.output == "json" {
					return writeJSON(a.Out, storage)
				}
				if !a.quiet {
					fmt.Fprintf(a.Out, "Storage: %s\nUUID: %s\n", storage.Name, storage.UUID)
				}
				return nil
			}
		}
		return fmt.Errorf("durable storage %q was not found", args[0])
	}}
}

func (a *App) storageRemoveCommand() *cobra.Command {
	command := &cobra.Command{Use: "remove <name>", Args: cobra.ExactArgs(1), Short: "Disable durable storage locally without deleting remote content", RunE: func(cmd *cobra.Command, args []string) error {
		repo, err := a.open()
		if err != nil {
			return err
		}
		defer repo.Close()
		if err := repo.DisableStorage(cmd.Context(), args[0]); err != nil {
			return err
		}
		if !a.quiet {
			fmt.Fprintf(a.Out, "Disabled durable storage %s on this peer; remote content was retained\n", args[0])
		}
		return nil
	}}
	a.addDryRun(command, "storage remove")
	return command
}

func (a *App) scopedHealthCommand() *cobra.Command {
	command := a.healthCommand()
	_ = command.Flags().MarkHidden("json")
	addScopeFlag(command)
	return command
}

func addScopeFlag(command *cobra.Command) {
	cluster := command.Flags().Lookup("cluster")
	if cluster == nil {
		return
	}
	scope := "local"
	command.Flags().StringVar(&scope, "scope", "local", "operation scope: local or cluster")
	_ = command.Flags().MarkHidden("cluster")
	previous := command.PreRunE
	command.PreRunE = func(cmd *cobra.Command, args []string) error {
		if previous != nil {
			if err := previous(cmd, args); err != nil {
				return err
			}
		}
		switch scope {
		case "local":
			return cluster.Value.Set("false")
		case "cluster":
			return cluster.Value.Set("true")
		default:
			return fmt.Errorf("unsupported scope %q; use local or cluster", scope)
		}
	}
}

func (a *App) consolidatedHistoryCommand() *cobra.Command {
	command := &cobra.Command{Use: "history", Short: "Inspect and restore namespace history"}
	list := a.historyCommand()
	list.Use = strings.Replace(list.Use, "history", "list", 1)
	restore := a.restoreCommand()
	a.addDryRun(restore, "history restore")
	conflicts := a.conflictsCommand()
	conflicts.Use = "conflicts"
	command.AddCommand(list, restore, conflicts)
	return command
}

func (a *App) consolidatedSyncCommand() *cobra.Command {
	command := a.syncCommand()
	metadataOnly := command.Flags().Lookup("metadata-only")
	mode := "full"
	command.Flags().StringVar(&mode, "mode", "full", "synchronization mode: metadata or full")
	_ = command.Flags().MarkHidden("metadata-only")
	previous := command.PreRunE
	command.PreRunE = func(cmd *cobra.Command, args []string) error {
		if previous != nil {
			if err := previous(cmd, args); err != nil {
				return err
			}
		}
		switch mode {
		case "metadata":
			return metadataOnly.Value.Set("true")
		case "full":
			return metadataOnly.Value.Set("false")
		default:
			return fmt.Errorf("unsupported sync mode %q; use metadata or full", mode)
		}
	}
	a.addDryRun(command, "sync")
	return command
}

func (a *App) addDryRun(command *cobra.Command, action string) {
	if command.Flags().Lookup("dry-run") != nil || command.RunE == nil {
		return
	}
	var dryRun bool
	command.Flags().BoolVar(&dryRun, "dry-run", false, "show the planned changes without applying them")
	run := command.RunE
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if dryRun {
			if a.output == "json" {
				return writeJSON(a.Out, map[string]any{"action": action, "dry_run": true, "arguments": args})
			}
			if !a.quiet {
				fmt.Fprintf(a.Out, "Dry run: %s %s\n", action, strings.Join(args, " "))
			}
			return nil
		}
		if scope, err := cmd.Flags().GetString("scope"); err == nil && scope == "cluster" {
			if err := a.confirm(cmd, action+" --scope cluster", 1); err != nil {
				return err
			}
		} else if command.Annotations != nil && command.Annotations["dfs-confirm"] == "true" {
			if err := a.confirm(cmd, action, 1); err != nil {
				return err
			}
		}
		return run(cmd, args)
	}
}

func markConfirmed(command *cobra.Command) {
	if command.Annotations == nil {
		command.Annotations = make(map[string]string)
	}
	command.Annotations["dfs-confirm"] = "true"
}

func (a *App) internalCommand() *cobra.Command {
	command := &cobra.Command{Use: "internal", Short: "Internal DFS runtime and recovery commands", Hidden: true}
	core := a.daemonCommand()
	core.Use = "core"
	mount := a.mountCommand()
	mount.Use = strings.Replace(mount.Use, "mount", "mount", 1)
	unmount := a.unmountCommand()
	transport := a.transportCommand()
	command.AddCommand(core, mount, unmount)
	for _, child := range detachChildren(transport, "git") {
		child.Use = strings.Replace(child.Use, "git", "transport-git", 1)
		command.AddCommand(child)
	}
	repository := &cobra.Command{Use: "repository", Hidden: true}
	init := a.initCommand()
	join := a.joinCommand()
	repository.AddCommand(init, join)
	command.AddCommand(repository)
	completeParent := a.networkCommand()
	pairing := &cobra.Command{Use: "pairing", Hidden: true}
	pairing.AddCommand(detachChildren(completeParent, "complete")...)
	command.AddCommand(pairing)
	return command
}

func (a *App) legacyRuntimeCommands() []*cobra.Command {
	daemon := a.daemonCommand()
	daemon.Hidden = true
	mount := a.mountCommand()
	mount.Hidden = true
	unmount := a.unmountCommand()
	unmount.Hidden = true
	transport := a.transportCommand()
	transport.Hidden = true
	for _, child := range transport.Commands() {
		if child.Name() != "git" {
			transport.RemoveCommand(child)
		}
	}
	return []*cobra.Command{daemon, mount, unmount, transport}
}

func writeJSON(output interface{ Write([]byte) (int, error) }, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = output.Write(append(data, '\n'))
	return err
}
