package client

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/config"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/version"
)

type Status struct {
	Configured      bool                      `json:"configured"`
	NetworkID       string                    `json:"networkId,omitempty"`
	NodeID          string                    `json:"nodeId,omitempty"`
	NodeName        string                    `json:"nodeName,omitempty"`
	GitHub          protocol.GitHubIdentity   `json:"github"`
	CurrentGitHub   protocol.GitHubIdentity   `json:"currentGithub"`
	CloudReachable  bool                      `json:"cloudReachable"`
	Warnings        []string                  `json:"warnings,omitempty"`
	Lease           protocol.CoordinatorLease `json:"lease"`
	RoutedProjects  int                       `json:"routedProjects"`
	LocalProjects   int                       `json:"localProjects"`
	IdentityDrift   bool                      `json:"identityDrift"`
	DriftReason     string                    `json:"driftReason,omitempty"`
	Membership      *protocol.Membership      `json:"membership,omitempty"`
	LastHeartbeatAt *time.Time                `json:"lastHeartbeatAt,omitempty"`
}

type Manager struct {
	statePath string
	config    config.Config
	repos     *storage.Repositories
	gh        *githubinfra.Gateway
	client    *http.Client
	now       func() time.Time
	mu        sync.RWMutex
	status    Status
	cancel    context.CancelFunc
	done      chan struct{}
	started   bool
}

func NewManager(statePath string, cfg config.Config, repos *storage.Repositories, gh *githubinfra.Gateway) *Manager {
	return &Manager{statePath: statePath, config: cfg, repos: repos, gh: gh, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now, done: make(chan struct{})}
}

func (m *Manager) Start(parent context.Context) error {
	if m.started {
		return nil
	}
	m.started = true
	state, err := LoadState(m.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			m.mu.Lock()
			m.status = Status{}
			m.mu.Unlock()
			close(m.done)
			return nil
		}
		m.started = false
		return err
	}
	m.mu.Lock()
	m.status = Status{Configured: true, NetworkID: state.NetworkID, NodeID: state.NodeID, NodeName: state.NodeName, GitHub: state.GitHub}
	m.mu.Unlock()
	routed, _ := countProjectModes(m.config)
	if routed == 0 {
		_, local := countProjectModes(m.config)
		m.mu.Lock()
		m.status.RoutedProjects = routed
		m.status.LocalProjects = local
		m.mu.Unlock()
		close(m.done)
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	go func() {
		defer close(m.done)
		m.tick(ctx, state)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.tick(ctx, state)
			}
		}
	}()
	return nil
}

func (m *Manager) Stop() {
	if !m.started {
		return
	}
	if m.cancel != nil {
		m.cancel()
	}
	<-m.done
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

func (m *Manager) tick(ctx context.Context, state LocalState) {
	current, _ := m.currentGitHubIdentity(ctx)
	routed, local := countProjectModes(m.config)
	capabilities := protocol.NodeCapabilities{Roles: supportedRoles(m.config), CoordinatorEligible: routed > 0 && m.config.Roles.Coordinator.Enabled, RoutedProjects: routed, LocalProjects: local, DynamicLoad: m.dynamicLoad(ctx)}
	drift, reason := identityDrift(state.GitHub, current)
	capabilities.IdentityDrift = drift
	capabilities.DriftReason = reason
	m.mu.Lock()
	m.status.IdentityDrift = drift
	m.status.DriftReason = reason
	m.mu.Unlock()
	api := New(state.URL, state.NodeToken, m.client)
	hb, err := api.Heartbeat(ctx, protocol.HeartbeatRequest{ProtocolVersion: protocol.CurrentVersion, DaemonVersion: version.Value, NodeName: state.NodeName, GitHub: current, Capabilities: capabilities})
	if err != nil {
		m.mu.Lock()
		m.status.CloudReachable = false
		m.status.CurrentGitHub = current
		m.status.RoutedProjects = routed
		m.status.LocalProjects = local
		m.mu.Unlock()
		return
	}
	m.mu.Lock()
	m.status.CloudReachable = true
	m.status.CurrentGitHub = current
	m.status.Warnings = hb.Warnings
	m.status.RoutedProjects = routed
	m.status.LocalProjects = local
	m.status.LastHeartbeatAt = &hb.RecordedAt
	m.mu.Unlock()
	status, err := api.Status(ctx)
	if err == nil {
		m.mu.Lock()
		m.status.Lease = status.Lease
		m.status.Membership = &status.Membership
		m.status.Warnings = status.Warnings
		m.mu.Unlock()
	}
}

func (m *Manager) currentGitHubIdentity(ctx context.Context) (protocol.GitHubIdentity, error) {
	if m.gh == nil {
		return protocol.GitHubIdentity{}, nil
	}
	identity, err := m.gh.GetCurrentUserIdentity(ctx, "")
	if err != nil {
		return protocol.GitHubIdentity{}, err
	}
	return protocol.GitHubIdentity{NumericID: identity.NumericID, Login: identity.Login}, nil
}

func (m *Manager) dynamicLoad(ctx context.Context) int {
	if m.repos == nil || m.repos.Runs == nil {
		return 0
	}
	runs, err := m.repos.Runs.List(ctx)
	if err != nil {
		return 0
	}
	total := 0
	for _, run := range runs {
		if run.Status == "running" {
			total++
		}
	}
	return total
}

func countProjectModes(cfg config.Config) (routed int, local int) {
	for _, project := range cfg.Projects {
		if project.Network.Mode == config.ProjectNetworkModeRouted {
			routed++
		} else {
			local++
		}
	}
	return routed, local
}

func supportedRoles(cfg config.Config) []string {
	roles := []string{}
	if cfg.Roles.Coordinator.Enabled {
		roles = append(roles, "coordinator")
	}
	if cfg.Roles.Worker.AutoDiscovery {
		roles = append(roles, "worker")
	}
	if cfg.Roles.Reviewer.Discovery.AutoDiscovery {
		roles = append(roles, "reviewer")
	}
	return roles
}

func identityDrift(expected, current protocol.GitHubIdentity) (bool, string) {
	if expected.NumericID > 0 && current.NumericID > 0 && expected.NumericID != current.NumericID {
		return true, fmt.Sprintf("stored GitHub numeric ID %d differs from current %d", expected.NumericID, current.NumericID)
	}
	if expected.Login != "" && current.Login != "" && expected.Login != current.Login {
		return true, fmt.Sprintf("stored GitHub login %q differs from current %q", expected.Login, current.Login)
	}
	return false, ""
}
