package collab

import (
	"encoding/json"

	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/storage"
)

type Status struct {
	Service          string               `json:"service"`
	PID              int                  `json:"pid"`
	State            string               `json:"state"`
	ActiveExecutions int                  `json:"activeExecutions"`
	Tasks            []Task               `json:"tasks"`
	Network          networkclient.Status `json:"network"`
	Plane            PlaneStatus          `json:"plane"`
}

type PlaneStatus struct {
	DeviceID       string `json:"deviceId,omitempty"`
	Reachable      bool   `json:"reachable"`
	RelayReachable bool   `json:"relayReachable"`
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
