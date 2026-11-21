package planestrict

import (
	"context"
	"encoding/json"
	"net/http"
)

type TelemetryAck struct {
	TaskID         string `json:"taskId"`
	Revision       int64  `json:"revision"`
	StoredRevision int64  `json:"storedRevision"`
}

// Historical observations use the node binding, never acquire an execution lease.
func (c *Client) UploadTelemetry(ctx context.Context, payload json.RawMessage) (TelemetryAck, error) {
	var ack TelemetryAck
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "telemetry", "batches"), nil, payload, nil, &ack)
	return ack, err
}
