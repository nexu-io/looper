package coordinator

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
)

type NetworkGateway interface {
	Status(context.Context) (protocol.NodeStatusResponse, error)
	RevalidateLease(context.Context, protocol.CoordinatorLeaseRevalidateRequest) error
}

type loopernetGateway struct {
	statePath string
	http      *http.Client
}

func NewLoopernetGateway(statePath string) NetworkGateway {
	if strings.TrimSpace(statePath) == "" {
		return nil
	}
	return &loopernetGateway{statePath: statePath, http: &http.Client{Timeout: 10 * time.Second}}
}

func (g *loopernetGateway) Status(ctx context.Context) (protocol.NodeStatusResponse, error) {
	client, err := g.client()
	if err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	return client.Status(ctx)
}

func (g *loopernetGateway) RevalidateLease(ctx context.Context, req protocol.CoordinatorLeaseRevalidateRequest) error {
	client, err := g.client()
	if err != nil {
		return err
	}
	return client.RevalidateLease(ctx, req)
}

func (g *loopernetGateway) client() (*client.Client, error) {
	state, err := client.LoadState(g.statePath)
	if err != nil {
		return nil, err
	}
	return client.New(state.URL, state.NodeToken, g.http), nil
}
