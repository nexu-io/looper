package cliapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/version"
	"github.com/spf13/cobra"
)

type networkStatusOutput struct {
	Configured     bool                       `json:"configured"`
	Membership     *protocol.Membership       `json:"membership,omitempty"`
	NodeName       string                     `json:"nodeName,omitempty"`
	GitHub         protocol.GitHubIdentity    `json:"github"`
	CurrentGitHub  protocol.GitHubIdentity    `json:"currentGithub"`
	Warnings       []string                   `json:"warnings,omitempty"`
	CloudReachable bool                       `json:"cloudReachable"`
	Lease          *protocol.CoordinatorLease `json:"lease,omitempty"`
	RoutedProjects int                        `json:"routedProjects"`
	LocalProjects  int                        `json:"localProjects"`
	IdentityDrift  bool                       `json:"identityDrift"`
	DriftReason    string                     `json:"driftReason,omitempty"`
}

func (r *commandRuntime) networkJoin(cmd *cobra.Command, args []string) error {
	url := strings.TrimSpace(args[0])
	joinKey := strings.TrimSpace(getStringFlag(cmd, "key"))
	nodeName := strings.TrimSpace(getStringFlag(cmd, "name"))
	if joinKey == "" {
		return fmt.Errorf("network join requires --key <key>")
	}
	if nodeName == "" {
		return fmt.Errorf("network join requires --name <name>")
	}
	if err := protocol.ValidateNodeName(nodeName); err != nil {
		return err
	}
	homeDir, err := r.homeDir()
	if err != nil {
		return err
	}
	identity, err := r.currentGitHubIdentity(cmd.Context())
	if err != nil {
		return err
	}
	cli := client.New(url, "", r.httpClient())
	joinResp, err := cli.Join(cmd.Context(), protocol.JoinRequest{
		ProtocolVersion: protocol.CurrentVersion,
		DaemonVersion:   version.Value,
		JoinKey:         joinKey,
		NodeName:        nodeName,
		GitHub:          identity,
		TargetLabels:    []string{protocol.TargetLabelForNode(nodeName)},
	})
	if err != nil {
		return err
	}
	state := client.LocalState{URL: url, NetworkID: joinResp.NetworkID, NodeID: joinResp.NodeID, NodeName: nodeName, NodeToken: joinResp.NodeToken, GitHub: identity}
	if err := client.SaveState(client.DefaultStatePath(homeDir), state); err != nil {
		return err
	}
	if !getBoolFlag(cmd, "no-enroll-projects") {
		if err := r.updateAllProjectNetworkModes(config.ProjectNetworkModeRouted); err != nil {
			if leaveErr := client.New(state.URL, state.NodeToken, r.httpClient()).Leave(cmd.Context()); leaveErr != nil {
				return errors.Join(err, leaveErr)
			}
			_ = client.RemoveState(client.DefaultStatePath(homeDir))
			return err
		}
	}
	output := map[string]any{"networkId": joinResp.NetworkID, "nodeId": joinResp.NodeID, "nodeName": nodeName, "github": identity, "warnings": joinResp.Warnings, "enrolledProjects": !getBoolFlag(cmd, "no-enroll-projects")}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	printSection(cmd.OutOrStdout(), "Network joined", [][2]any{{"networkId", joinResp.NetworkID}, {"nodeId", joinResp.NodeID}, {"nodeName", nodeName}, {"githubLogin", identity.Login}, {"githubNumericId", identity.NumericID}, {"enrolledProjects", !getBoolFlag(cmd, "no-enroll-projects")}, {"warnings", joinOrNone(joinResp.Warnings)}})
	return nil

}

func (r *commandRuntime) networkLeave(cmd *cobra.Command, args []string) error {
	_ = args
	state, path, err := r.loadNetworkState()
	if err != nil {
		return err
	}
	cli := client.New(state.URL, state.NodeToken, r.httpClient())
	if err := cli.Leave(cmd.Context()); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
			return err
		}
	}
	if err := r.updateAllProjectNetworkModes(config.ProjectNetworkModeOff); err != nil {
		if removeErr := client.RemoveState(path); removeErr != nil && !isNotExist(removeErr) {
			return errors.Join(err, removeErr)
		}
		return err
	}
	if err := client.RemoveState(path); err != nil {
		return err
	}
	output := map[string]any{"left": true, "restartRequired": true}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	printSection(cmd.OutOrStdout(), "Network left", [][2]any{{"left", true}, {"restartRequired", true}, {"message", "Restart looperd to fully apply local network changes."}})
	return nil
}

func (r *commandRuntime) networkStatus(cmd *cobra.Command, args []string) error {
	_ = args
	status, err := r.resolveNetworkStatus(cmd.Context())
	if err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), status)
	}
	return writeHumanNetworkStatus(cmd.OutOrStdout(), status, getBoolFlag(cmd, "verbose"))
}

func (r *commandRuntime) resolveNetworkStatus(ctx context.Context) (networkStatusOutput, error) {
	state, _, err := r.loadNetworkState()
	if err != nil {
		if isNotExist(err) {
			local, routed, current, currentErr := r.localProjectCountsAndIdentity(ctx)
			if currentErr != nil {
				return networkStatusOutput{}, currentErr
			}
			return networkStatusOutput{Configured: false, CloudReachable: false, GitHub: protocol.GitHubIdentity{}, CurrentGitHub: current, LocalProjects: local, RoutedProjects: routed}, nil
		}
		return networkStatusOutput{}, err
	}
	current, err := r.currentGitHubIdentity(ctx)
	if err != nil {
		return networkStatusOutput{}, err
	}
	local, routed, _, err := r.localProjectCountsAndIdentity(ctx)
	if err != nil {
		return networkStatusOutput{}, err
	}
	status := networkStatusOutput{Configured: true, NodeName: state.NodeName, GitHub: state.GitHub, CurrentGitHub: current, LocalProjects: local, RoutedProjects: routed}
	if drift, reason := githubIdentityDrift(state.GitHub, current); drift {
		status.IdentityDrift = true
		status.DriftReason = reason
	}
	remote, remoteErr := client.New(state.URL, state.NodeToken, r.httpClient()).Status(ctx)
	if remoteErr == nil {
		status.CloudReachable = true
		status.Lease = &remote.Lease
		membership := remote.Membership
		status.Membership = &membership
		status.Warnings = append([]string{}, remote.Warnings...)
		if remote.IdentityDrift {
			status.IdentityDrift = true
			status.DriftReason = remote.IdentityDriftReason
		}
	}
	return status, nil
}

func (r *commandRuntime) loadNetworkState() (client.LocalState, string, error) {
	homeDir, err := r.homeDir()
	if err != nil {
		return client.LocalState{}, "", err
	}
	path := client.DefaultStatePath(homeDir)
	state, err := client.LoadState(path)
	return state, path, err
}

func (r *commandRuntime) currentGitHubIdentity(ctx context.Context) (protocol.GitHubIdentity, error) {
	cwd, err := r.getwd()
	if err != nil {
		return protocol.GitHubIdentity{}, err
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: r.argv})
	if err != nil {
		return protocol.GitHubIdentity{}, err
	}
	gh := githubinfra.New(githubinfra.Options{GHPath: networkStringValue(loaded.Config.Tools.GHPath)})
	identity, err := gh.GetCurrentUserIdentity(ctx, "")
	if err != nil {
		return protocol.GitHubIdentity{}, err
	}
	return protocol.GitHubIdentity{NumericID: identity.NumericID, Login: identity.Login}, nil
}

func (r *commandRuntime) localProjectCountsAndIdentity(ctx context.Context) (local int, routed int, current protocol.GitHubIdentity, err error) {
	cwd, cwdErr := r.getwd()
	if cwdErr != nil {
		return 0, 0, protocol.GitHubIdentity{}, cwdErr
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: r.argv})
	if err != nil {
		return 0, 0, protocol.GitHubIdentity{}, err
	}
	for _, project := range loaded.Config.Projects {
		if project.Network.Mode == config.ProjectNetworkModeRouted {
			routed++
		} else {
			local++
		}
	}
	current, err = r.currentGitHubIdentity(ctx)
	return local, routed, current, err
}

func (r *commandRuntime) updateAllProjectNetworkModes(mode config.ProjectNetworkMode) error {
	cwd, err := r.getwd()
	if err != nil {
		return err
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: r.argv})
	if err != nil {
		return err
	}
	for index := range loaded.Config.Projects {
		loaded.Config.Projects[index].Network = config.ProjectNetworkConfig{Mode: mode}
		if mode == config.ProjectNetworkModeOff {
			loaded.Config.Projects[index].Network = config.ProjectNetworkConfig{}
		}
	}
	partial := loaded.Partial
	projects := partial.Projects
	if projects == nil {
		materialized := make([]config.PartialProjectRefConfig, len(loaded.Config.Projects))
		for index, project := range loaded.Config.Projects {
			materialized[index] = config.PartialProjectRefConfig{ID: project.ID, Name: project.Name, RepoPath: project.RepoPath, Path: project.Path, BaseBranch: project.BaseBranch, WorktreeRoot: project.WorktreeRoot, Roles: project.Roles}
			if project.Network.Mode != config.ProjectNetworkModeOff {
				projectMode := project.Network.Mode
				materialized[index].Network = &config.PartialProjectNetworkConfig{Mode: &projectMode}
			}
		}
		projects = &materialized
	}
	for index := range *projects {
		(*projects)[index].Network = nil
		if mode != config.ProjectNetworkModeOff {
			projectMode := config.NetworkMode(mode)
			(*projects)[index].Network = &config.PartialProjectNetworkConfig{Mode: &projectMode}
		}
	}
	partial.Projects = projects
	raw, err := config.MarshalConfigFile(loaded.Metadata.ConfigPath, partial)
	if err != nil {
		return err
	}
	if _, err := config.Normalize(cwd, partial); err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(loaded.Metadata.ConfigPath), 0o755); err != nil {
		return err
	}
	tmp := loaded.Metadata.ConfigPath + ".tmp"
	if err := r.writeFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return renameFile(tmp, loaded.Metadata.ConfigPath)
}

func githubIdentityDrift(expected, current protocol.GitHubIdentity) (bool, string) {
	if expected.NumericID > 0 && current.NumericID > 0 && expected.NumericID != current.NumericID {
		return true, fmt.Sprintf("stored GitHub numeric ID %d differs from current %d", expected.NumericID, current.NumericID)
	}
	if strings.TrimSpace(expected.Login) != "" && strings.TrimSpace(current.Login) != "" && !strings.EqualFold(expected.Login, current.Login) {
		return true, fmt.Sprintf("stored GitHub login %q differs from current %q", expected.Login, current.Login)
	}
	return false, ""
}

func writeHumanNetworkStatus(w io.Writer, status networkStatusOutput, verbose bool) error {
	printSection(w, "Network", [][2]any{{"configured", status.Configured}, {"cloudReachable", status.CloudReachable}, {"nodeName", status.NodeName}, {"githubLogin", status.GitHub.Login}, {"githubNumericId", status.GitHub.NumericID}, {"currentGithubLogin", status.CurrentGitHub.Login}, {"currentGithubNumericId", status.CurrentGitHub.NumericID}, {"identityDrift", status.IdentityDrift}, {"routedProjects", status.RoutedProjects}, {"localProjects", status.LocalProjects}})
	if status.DriftReason != "" {
		fmt.Fprintln(w)
		printSection(w, "Identity drift", [][2]any{{"reason", status.DriftReason}})
	}
	if status.Lease != nil {
		fmt.Fprintln(w)
		printSection(w, "Coordinator lease", [][2]any{{"holderNodeId", status.Lease.HolderNodeID}, {"fencingToken", status.Lease.FencingToken}, {"expiresAt", status.Lease.ExpiresAt}})
	}
	if verbose && status.Membership != nil {
		fmt.Fprintln(w)
		printSection(w, "Membership", [][2]any{{"nodeId", status.Membership.NodeID}, {"nodeName", status.Membership.NodeName}, {"duplicateIdentityWarning", status.Membership.DuplicateWarning}, {"coordinatorEligible", status.Membership.Capabilities.CoordinatorEligible}, {"roles", joinOrNone(status.Membership.Capabilities.Roles)}, {"dynamicLoad", status.Membership.Capabilities.DynamicLoad}, {"lastHeartbeatAt", status.Membership.LastHeartbeatAt}})
	}
	if len(status.Warnings) > 0 {
		fmt.Fprintln(w)
		entries := make([][2]any, 0, len(status.Warnings))
		for index, warning := range status.Warnings {
			entries = append(entries, [2]any{fmt.Sprintf("warning[%d]", index), warning})
		}
		printSection(w, "Warnings", entries)
	}
	return nil
}

func networkStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isNotExist(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such file")
}

func renameFile(from, to string) error {
	return os.Rename(from, to)
}
