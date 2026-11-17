package collab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/storage"
)

func (h *bridgeHost) planeSources() []planeSource {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]planeSource(nil), h.sources...)
}

func (h *bridgeHost) loadPersonalPlane(networkPath string) error {
	connection, err := planestrict.LoadDeviceConnection(filepath.Join(filepath.Dir(networkPath), "plane-device.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	client, err := connection.Client(connection.Workspace, connection.ProjectID)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.personalPlane, h.personalClient = &connection, client
	h.planeStatus.DeviceID = connection.DeviceID
	h.mu.Unlock()
	h.personalSources = make(map[string]planeSource)
	return nil
}

func (h *bridgeHost) refreshPersonalPlane(ctx context.Context) error {
	if h.personalClient == nil {
		if h.personalPath != "" {
			if err := h.loadPersonalPlane(h.personalPath); err != nil {
				return err
			}
		}
		if h.personalClient == nil {
			return nil
		}
	}
	listing, err := h.personalClient.DeviceProjects(ctx)
	h.mu.Lock()
	h.planeStatus.Reachable = err == nil && listing.State == "active"
	h.mu.Unlock()
	if err != nil {
		return err
	}
	allowed := make(map[string]bool)
	if listing.State == "active" {
		for _, project := range listing.Projects {
			allowed["personal-plane-"+project.ID] = true
		}
	}
	// A successful directory response is the current execution authority. A
	// network error above is not a revocation. Stop retained sessions as well as
	// running tasks when the owner disconnects or loses project membership.
	items, err := h.repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	for _, loop := range items {
		if loop.Status != "terminated" && strings.HasPrefix(loop.ProjectID, "personal-plane-") && !allowed[loop.ProjectID] {
			if _, err := h.stopBridgeTask(ctx, loop); err != nil {
				return err
			}
		}
	}
	var sources []planeSource
	for _, project := range listing.Projects {
		source, exists := h.personalSources[project.ID]
		if !exists {
			client, err := forge.NewDevicePlaneClient(*h.personalPlane, project.Workspace, project.ID)
			if err != nil {
				return err
			}
			source = planeSource{project: config.ProjectRefConfig{ID: "personal-plane-" + project.ID, Provider: project.Workspace + ":" + project.ID}, client: client, nodeID: h.personalPlane.NodeID}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if err := h.repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: source.project.ID, Name: project.Name,
				RepoPath: filepath.Join(h.dir, "workspace"), CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			h.personalSources[project.ID] = source
		}
		sources = append(sources, source)
	}
	h.mu.Lock()
	h.sources = append(append([]planeSource(nil), h.legacySources...), sources...)
	h.mu.Unlock()
	return nil
}

func (h *bridgeHost) heartbeatPersonalPlane(ctx context.Context) error {
	h.mu.Lock()
	client := h.personalClient
	direct := h.personalPlane != nil && h.personalPlane.Pairing == "device_v1"
	h.mu.Unlock()
	if client == nil || !direct {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := client.DeviceHeartbeat(ctx)
	h.mu.Lock()
	h.planeStatus.HeartbeatReachable = err == nil
	h.mu.Unlock()
	return err
}

func (h *bridgeHost) maintainPersonalHeartbeat(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := h.heartbeatPersonalPlane(ctx); err != nil {
			h.warn("Plane device heartbeat failed; Plane task polling continues", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
