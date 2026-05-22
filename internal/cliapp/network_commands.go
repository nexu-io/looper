package cliapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/version"
	"github.com/spf13/cobra"
)

type networkStatusOutput struct {
	Configured       bool                       `json:"configured"`
	NetworkID        string                     `json:"networkId,omitempty"`
	Membership       *protocol.Membership       `json:"membership,omitempty"`
	LeaseHolder      *protocol.Membership       `json:"leaseHolder,omitempty"`
	Memberships      []protocol.Membership      `json:"memberships,omitempty"`
	NodeName         string                     `json:"nodeName,omitempty"`
	GitHub           protocol.GitHubIdentity    `json:"github"`
	CurrentGitHub    protocol.GitHubIdentity    `json:"currentGithub"`
	Warnings         []string                   `json:"warnings,omitempty"`
	CloudReachable   bool                       `json:"cloudReachable"`
	Lease            *protocol.CoordinatorLease `json:"lease,omitempty"`
	RoutedProjects   int                        `json:"routedProjects"`
	LocalProjects    int                        `json:"localProjects"`
	IdentityDrift    bool                       `json:"identityDrift"`
	IdentityFallback bool                       `json:"identityFallback,omitempty"`
	LeaseAction      string                     `json:"leaseAction,omitempty"`
	LeaseError       string                     `json:"leaseError,omitempty"`
	DriftReason      string                     `json:"driftReason,omitempty"`
}

type networkMembersOutput struct {
	NetworkID         string                 `json:"networkId"`
	CurrentNodeID     string                 `json:"currentNodeId,omitempty"`
	CurrentNodeName   string                 `json:"currentNodeName,omitempty"`
	LeaseHolderNodeID string                 `json:"leaseHolderNodeId,omitempty"`
	LeaseHolderName   string                 `json:"leaseHolderNodeName,omitempty"`
	Members           []networkMemberSummary `json:"members"`
	Warnings          []string               `json:"warnings,omitempty"`
}

type networkMemberSummary struct {
	NodeID                   string                  `json:"nodeId"`
	NodeName                 string                  `json:"nodeName"`
	GitHub                   protocol.GitHubIdentity `json:"github"`
	Roles                    []string                `json:"roles,omitempty"`
	RoutedProjects           int                     `json:"routedProjects"`
	LocalProjects            int                     `json:"localProjects"`
	LastHeartbeatAt          *string                 `json:"lastHeartbeatAt,omitempty"`
	Current                  bool                    `json:"current,omitempty"`
	LeaseHolder              bool                    `json:"leaseHolder,omitempty"`
	TargetLabels             []string                `json:"targetLabels,omitempty"`
	CoordinatorEligible      bool                    `json:"coordinatorEligible,omitempty"`
	DynamicLoad              int                     `json:"dynamicLoad,omitempty"`
	JoinedAt                 string                  `json:"joinedAt,omitempty"`
	DuplicateIdentityWarning bool                    `json:"duplicateGithubIdentityWarning,omitempty"`
}

func (r *commandRuntime) networkJoin(cmd *cobra.Command, args []string) error {
	url := strings.TrimSpace(args[0])
	joinKey := strings.TrimSpace(getStringFlag(cmd, "key"))
	nodeName := strings.TrimSpace(getStringFlag(cmd, "name"))
	autoEnrollProjects := !getBoolFlag(cmd, "no-enroll-projects")
	if joinKey == "" {
		return fmt.Errorf("network join requires --key <key>")
	}
	if nodeName == "" {
		return fmt.Errorf("network join requires --name <name>")
	}
	if err := protocol.ValidateNodeName(nodeName); err != nil {
		return err
	}
	if autoEnrollProjects {
		if err := r.validateRoutedAutoEnrollment(); err != nil {
			return err
		}
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
	if autoEnrollProjects {
		if err := r.updateAllProjectNetworkModes(config.ProjectNetworkModeRouted); err != nil {
			if leaveErr := client.New(state.URL, state.NodeToken, r.httpClient()).Leave(cmd.Context()); leaveErr != nil {
				return errors.Join(err, leaveErr)
			}
			_ = client.RemoveState(client.DefaultStatePath(homeDir))
			return err
		}
	}
	output := map[string]any{"networkId": joinResp.NetworkID, "nodeId": joinResp.NodeID, "nodeName": nodeName, "github": identity, "warnings": joinResp.Warnings, "enrolledProjects": autoEnrollProjects}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	printSection(cmd.OutOrStdout(), "Network joined", [][2]any{{"networkId", joinResp.NetworkID}, {"nodeId", joinResp.NodeID}, {"nodeName", nodeName}, {"githubLogin", identity.Login}, {"githubNumericId", identity.NumericID}, {"enrolledProjects", autoEnrollProjects}, {"warnings", joinOrNone(joinResp.Warnings)}})
	return nil

}

func (r *commandRuntime) validateRoutedAutoEnrollment() error {
	cwd, err := r.getwd()
	if err != nil {
		return err
	}
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: ExtractConfigArgs(r.argv)})
	if err != nil {
		return err
	}
	var affected []string
	for _, project := range loaded.Config.Projects {
		roles := config.ProjectRoleConfigs(loaded.Config, project.ID)
		var unsupported []string
		if roles.Planner.AutoDiscovery {
			unsupported = append(unsupported, "roles.planner.autoDiscovery")
		}
		if roles.Fixer.AutoDiscovery {
			unsupported = append(unsupported, "roles.fixer.autoDiscovery")
		}
		if len(unsupported) == 0 {
			continue
		}
		affected = append(affected, fmt.Sprintf("%s (%s)", networkProjectDisplayName(project), strings.Join(unsupported, ", ")))
	}
	if len(affected) == 0 {
		return nil
	}
	return fmt.Errorf("cannot auto-enroll projects in network.mode=routed while planner/fixer auto-discovery is enabled: %s; disable those settings globally or per-project, or rerun network join with --no-enroll-projects and opt projects into routed mode manually", strings.Join(affected, "; "))
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

func (r *commandRuntime) networkMembers(cmd *cobra.Command, args []string) error {
	_ = args
	members, err := r.resolveNetworkMembers(cmd.Context())
	if err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), members)
	}
	return writeHumanNetworkMembers(cmd.OutOrStdout(), members, getBoolFlag(cmd, "verbose"))
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
	status := networkStatusOutput{Configured: true, NetworkID: state.NetworkID, NodeName: state.NodeName, GitHub: state.GitHub, CurrentGitHub: current, LocalProjects: local, RoutedProjects: routed}
	status.IdentityFallback = current.Login != "" && current.NumericID == 0
	if drift, reason := githubIdentityDrift(state.GitHub, current); drift {
		status.IdentityDrift = true
		status.DriftReason = reason
	}
	remote, remoteErr := client.New(state.URL, state.NodeToken, r.httpClient()).Status(ctx)
	if remoteErr == nil {
		status.CloudReachable = true
		if strings.TrimSpace(remote.NetworkID) != "" {
			status.NetworkID = remote.NetworkID
		}
		status.Lease = &remote.Lease
		membership := remote.Membership
		status.Membership = &membership
		status.Memberships = append([]protocol.Membership{}, remote.Memberships...)
		status.LeaseHolder = findMembershipByNodeID(remote.Memberships, remote.Lease.HolderNodeID)
		status.Warnings = append([]string{}, remote.Warnings...)
		if remote.IdentityDrift {
			status.IdentityDrift = true
			status.DriftReason = remote.IdentityDriftReason
		}
	}
	return status, nil
}

func (r *commandRuntime) resolveNetworkMembers(ctx context.Context) (networkMembersOutput, error) {
	state, _, err := r.loadNetworkState()
	if err != nil {
		if isNotExist(err) {
			return networkMembersOutput{}, fmt.Errorf("network members requires a joined network")
		}
		return networkMembersOutput{}, err
	}
	remote, err := client.New(state.URL, state.NodeToken, r.httpClient()).Status(ctx)
	if err != nil {
		if isNetworkStatusReachabilityError(err) {
			return networkMembersOutput{}, fmt.Errorf("network members requires a reachable loopernet status endpoint: %w", err)
		}
		return networkMembersOutput{}, err
	}
	leaseHolder := findMembershipByNodeID(remote.Memberships, remote.Lease.HolderNodeID)
	output := networkMembersOutput{
		NetworkID:         remote.NetworkID,
		CurrentNodeID:     remote.Membership.NodeID,
		CurrentNodeName:   remote.Membership.NodeName,
		LeaseHolderNodeID: remote.Lease.HolderNodeID,
		Warnings:          append([]string{}, remote.Warnings...),
	}
	if leaseHolder != nil {
		output.LeaseHolderName = leaseHolder.NodeName
	}
	for _, member := range remote.Memberships {
		summary := networkMemberSummary{
			NodeID:                   member.NodeID,
			NodeName:                 member.NodeName,
			GitHub:                   member.GitHub,
			Roles:                    append([]string{}, member.Capabilities.Roles...),
			RoutedProjects:           member.Capabilities.RoutedProjects,
			LocalProjects:            member.Capabilities.LocalProjects,
			Current:                  member.NodeID == remote.Membership.NodeID,
			LeaseHolder:              leaseHolder != nil && member.NodeID == leaseHolder.NodeID,
			TargetLabels:             append([]string{}, member.TargetLabels...),
			CoordinatorEligible:      member.Capabilities.CoordinatorEligible,
			DynamicLoad:              member.Capabilities.DynamicLoad,
			JoinedAt:                 member.JoinedAt.Format(time.RFC3339),
			DuplicateIdentityWarning: member.DuplicateWarning,
		}
		if member.LastHeartbeatAt != nil {
			value := member.LastHeartbeatAt.Format(time.RFC3339)
			summary.LastHeartbeatAt = &value
		}
		output.Members = append(output.Members, summary)
	}
	if strings.TrimSpace(output.NetworkID) == "" {
		output.NetworkID = state.NetworkID
	}
	return output, nil
}

func isNetworkStatusReachabilityError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
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
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: ExtractConfigArgs(r.argv)})
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
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: ExtractConfigArgs(r.argv)})
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
	loaded, err := config.LoadFile(config.LoadFileOptions{CWD: cwd, Args: ExtractConfigArgs(r.argv)})
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

func networkProjectDisplayName(project config.ProjectRefConfig) string {
	if trimmed := strings.TrimSpace(project.ID); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(project.Name); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(project.RepoPath); trimmed != "" {
		return trimmed
	}
	return "<unnamed project>"
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
	printSection(w, "Network", [][2]any{{"configured", status.Configured}, {"networkId", status.NetworkID}, {"cloudReachable", status.CloudReachable}, {"nodeName", status.NodeName}, {"githubLogin", status.GitHub.Login}, {"githubNumericId", status.GitHub.NumericID}, {"currentGithubLogin", status.CurrentGitHub.Login}, {"currentGithubNumericId", status.CurrentGitHub.NumericID}, {"identityDrift", status.IdentityDrift}, {"routedProjects", status.RoutedProjects}, {"localProjects", status.LocalProjects}})
	if status.IdentityFallback {
		fmt.Fprintln(w)
		printSection(w, "Identity fallback", [][2]any{{"warning", "current GitHub identity is using login fallback because no numeric ID is available"}})
	}
	if status.DriftReason != "" {
		fmt.Fprintln(w)
		printSection(w, "Identity drift", [][2]any{{"reason", status.DriftReason}})
	}
	if status.Lease != nil {
		fmt.Fprintln(w)
		rows := [][2]any{{"holderNodeId", status.Lease.HolderNodeID}, {"fencingToken", status.Lease.FencingToken}, {"expiresAt", status.Lease.ExpiresAt}, {"leaseAction", status.LeaseAction}, {"leaseError", status.LeaseError}}
		if status.LeaseHolder != nil {
			rows = append(rows, [2]any{"holderNodeName", status.LeaseHolder.NodeName}, [2]any{"holderGithubLogin", status.LeaseHolder.GitHub.Login})
		}
		printSection(w, "Coordinator lease", rows)
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

func writeHumanNetworkMembers(w io.Writer, output networkMembersOutput, verbose bool) error {
	printSection(w, "Network members", [][2]any{{"networkId", output.NetworkID}, {"currentNodeId", output.CurrentNodeID}, {"currentNodeName", output.CurrentNodeName}, {"leaseHolderNodeId", output.LeaseHolderNodeID}, {"leaseHolderNodeName", output.LeaseHolderName}, {"count", len(output.Members)}})
	if len(output.Members) > 0 {
		fmt.Fprintln(w)
		rows := make([]tableRow, 0, len(output.Members))
		for _, member := range output.Members {
			rows = append(rows, tableRow{
				"nodeId":        member.NodeID,
				"node":          member.NodeName,
				"github":        member.GitHub.Login,
				"roles":         joinOrNone(member.Roles),
				"routed":        member.RoutedProjects,
				"local":         member.LocalProjects,
				"lastHeartbeat": valueOrNone(member.LastHeartbeatAt),
				"current":       member.Current,
				"lease":         member.LeaseHolder,
			})
		}
		printTable(w, []string{"nodeId", "node", "github", "roles", "routed", "local", "lastHeartbeat", "current", "lease"}, rows)
	}
	if verbose {
		for index, member := range output.Members {
			fmt.Fprintln(w)
			rows := [][2]any{{"nodeId", member.NodeID}, {"nodeName", member.NodeName}, {"githubLogin", member.GitHub.Login}, {"githubNumericId", member.GitHub.NumericID}, {"current", member.Current}, {"leaseHolder", member.LeaseHolder}, {"roles", joinOrNone(member.Roles)}, {"routedProjects", member.RoutedProjects}, {"localProjects", member.LocalProjects}, {"lastHeartbeatAt", valueOrNone(member.LastHeartbeatAt)}, {"targetLabels", joinOrNone(member.TargetLabels)}, {"coordinatorEligible", member.CoordinatorEligible}, {"dynamicLoad", member.DynamicLoad}, {"joinedAt", member.JoinedAt}, {"duplicateGithubIdentityWarning", member.DuplicateIdentityWarning}}
			printSection(w, fmt.Sprintf("Member %d", index+1), rows)
		}
	}
	if len(output.Warnings) > 0 {
		fmt.Fprintln(w)
		entries := make([][2]any, 0, len(output.Warnings))
		for index, warning := range output.Warnings {
			entries = append(entries, [2]any{fmt.Sprintf("warning[%d]", index), warning})
		}
		printSection(w, "Warnings", entries)
	}
	return nil
}

func valueOrNone(value *string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "none"
	}
	return *value
}

func networkStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func findMembershipByNodeID(memberships []protocol.Membership, nodeID string) *protocol.Membership {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return nil
	}
	for _, member := range memberships {
		if strings.TrimSpace(member.NodeID) == nodeID {
			copy := member
			return &copy
		}
	}
	return nil
}

func isNotExist(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such file")
}

func renameFile(from, to string) error {
	return os.Rename(from, to)
}
