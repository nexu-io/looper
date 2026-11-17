package runtime

import (
	"context"
	"sync"
)

type blockingWebhookTunnelGitHubClient struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (c blockingWebhookTunnelGitHubClient) GetHook(context.Context, string, int64) (webhookTunnelGitHubHook, bool, error) {
	return webhookTunnelGitHubHook{}, false, nil
}

func (c blockingWebhookTunnelGitHubClient) CreateHook(ctx context.Context, _ string, _ string, _ string, _ []string) (webhookTunnelGitHubHook, error) {
	close(c.started)
	select {
	case <-c.release:
		return webhookTunnelGitHubHook{ID: 1}, nil
	case <-ctx.Done():
		return webhookTunnelGitHubHook{}, ctx.Err()
	}
}

func (c blockingWebhookTunnelGitHubClient) UpdateHook(context.Context, string, int64, string, string, []string, bool) (webhookTunnelGitHubHook, error) {
	return webhookTunnelGitHubHook{}, nil
}

func (c blockingWebhookTunnelGitHubClient) DeleteHook(context.Context, string, int64) error {
	return nil
}

// recordingBlockingTunnelClient blocks only the first CreateHook, remembers
// created hooks for later GetHook, and signals when a second create arrives.
type recordingBlockingTunnelClient struct {
	mu               sync.Mutex
	hooks            map[string]webhookTunnelGitHubHook
	createOrder      []string
	nextID           int64
	firstStarted     chan struct{}
	firstRelease     <-chan struct{}
	secondCreateSeen chan struct{}
	firstSignaled    bool
}

func (c *recordingBlockingTunnelClient) createRepos() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.createOrder))
	copy(out, c.createOrder)
	return out
}

func (c *recordingBlockingTunnelClient) GetHook(_ context.Context, repo string, id int64) (webhookTunnelGitHubHook, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hook, ok := c.hooks[repo]
	if !ok || hook.ID != id {
		return webhookTunnelGitHubHook{}, false, nil
	}
	return hook, true, nil
}

func (c *recordingBlockingTunnelClient) CreateHook(ctx context.Context, repo string, url string, _ string, events []string) (webhookTunnelGitHubHook, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	hook := webhookTunnelGitHubHook{ID: id, Active: true, Events: append([]string(nil), events...)}
	hook.Config.URL = url
	hook.Config.ContentType = "json"
	hook.Config.InsecureSSL = "0"
	if c.hooks == nil {
		c.hooks = map[string]webhookTunnelGitHubHook{}
	}
	c.hooks[repo] = hook
	c.createOrder = append(c.createOrder, repo)
	createIndex := len(c.createOrder)
	first := !c.firstSignaled
	if first {
		c.firstSignaled = true
	}
	c.mu.Unlock()

	if first {
		close(c.firstStarted)
		select {
		case <-c.firstRelease:
		case <-ctx.Done():
			return webhookTunnelGitHubHook{}, ctx.Err()
		}
	} else if createIndex == 2 {
		select {
		case <-c.secondCreateSeen:
		default:
			close(c.secondCreateSeen)
		}
	}
	return hook, nil
}

func (c *recordingBlockingTunnelClient) UpdateHook(_ context.Context, repo string, id int64, url string, _ string, events []string, active bool) (webhookTunnelGitHubHook, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hook := webhookTunnelGitHubHook{ID: id, Active: active, Events: append([]string(nil), events...)}
	hook.Config.URL = url
	hook.Config.ContentType = "json"
	hook.Config.InsecureSSL = "0"
	if c.hooks == nil {
		c.hooks = map[string]webhookTunnelGitHubHook{}
	}
	c.hooks[repo] = hook
	return hook, nil
}

func (c *recordingBlockingTunnelClient) DeleteHook(context.Context, string, int64) error {
	return nil
}
