package planestrict

import (
	"context"
	"encoding/json"
	"net/http"
)

func (c *Client) Memory(ctx context.Context, input map[string]any) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "memories", "tool"), nil, input, nil, &result)
	return result, err
}
