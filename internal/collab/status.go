package collab

import (
	"encoding/json"

	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/storage"
)

type Status struct {
	PlaneTargets     []PlaneTarget        `json:"planeTargets"`
	Service          string               `json:"service"`
	PID              int                  `json:"pid"`
	State            string               `json:"state"`
	ActiveExecutions int                  `json:"activeExecutions"`
	Tasks            []Task               `json:"tasks"`
	Network          networkclient.Status `json:"network"`
}

// Task is a presentation of existing loop records, not another task state.
// Prompts, checkpoints and execution parameters stay out of status responses.
type Task struct {
	ID           string   `json:"id"`
	Seq          int64    `json:"seq"`
	ProjectID    string   `json:"projectId"`
	Title        string   `json:"title"`
	Repo         string   `json:"repo"`
	Status       string   `json:"status"`
	PRNumber     *int64   `json:"prNumber,omitempty"`
	PullRequests []string `json:"pullRequests,omitempty"`
}

func taskStatus(record storage.LoopRecord) Task {
	var metadata struct {
		Worker struct {
			Title string
			Repo  string
		} `json:"worker"`
	}
	if record.MetadataJSON != nil {
		_ = json.Unmarshal([]byte(*record.MetadataJSON), &metadata)
	}
	repo := metadata.Worker.Repo
	if record.Repo != nil {
		repo = *record.Repo
	}
	_, bridge, _ := readBridgeTask(record)
	return Task{ID: record.ID, Seq: record.Seq, ProjectID: record.ProjectID, Title: metadata.Worker.Title, Repo: repo, Status: record.Status, PRNumber: record.PRNumber, PullRequests: bridge.Outcome.PullRequests}
}

// PlaneTarget advertises a loaded connection; it does not select implementation repositories.
type PlaneTarget struct {
	Workspace string `json:"workspace"`
	ProjectID string `json:"projectId"`
	Repo      string `json:"repo,omitempty"`
}

func configuredPlaneTargets(cfg config.Config) []PlaneTarget {
	var out []PlaneTarget
	for _, provider := range cfg.Providers {
		if provider.Kind == config.ProviderKindPlane {
			out = append(out, PlaneTarget{Workspace: value(provider.Workspace), ProjectID: value(provider.ProjectID)})
		}
	}
	return out
}
