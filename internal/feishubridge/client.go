package feishubridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Init struct {
	MemorySocket   string `json:"memorySocket,omitempty"`
	MemoryScript   string `json:"memoryScript,omitempty"`
	Socket         string `json:"socket"`
	CallbackSocket string `json:"callbackSocket"`
	CWD            string `json:"cwd"`
	AppID          string `json:"appId"`
	AppSecret      string `json:"appSecret"`
	OwnerOpenID    string `json:"ownerOpenId"`
	Concurrency    int    `json:"concurrency,omitempty"`
	ExternalEvents bool   `json:"externalEvents,omitempty"`
}

type Task struct {
	ChatID          string `json:"chatId"`
	ThreadID        string `json:"threadId,omitempty"`
	RootMessageID   string `json:"rootMessageId"`
	CWD             string `json:"cwd"`
	Title           string `json:"title"`
	Prompt          string `json:"prompt"`
	RequesterOpenID string `json:"requesterOpenId,omitempty"`
	BackendID       string `json:"backendId,omitempty"`
	Model           string `json:"model,omitempty"`
}

type TurnEvent struct {
	TurnID        string `json:"turnId"`
	RootMessageID string `json:"rootMessageId"`
	Phase         string `json:"phase"`
	ChatID        string `json:"chatId"`
	ThreadID      string `json:"threadId"`
	SessionID     string `json:"sessionId"`
	CWD           string `json:"cwd"`
	Terminal      string `json:"terminal"`
	Text          string `json:"text"`
}

type Client struct {
	socket string
	cmd    *exec.Cmd
	pipe   io.WriteCloser
	done   chan struct{}
	once   sync.Once
	err    error
}

func Start(ctx context.Context, entry, home string, init Init, log io.Writer, privateSecrets ...string) (*Client, error) {
	if init.AppID == "" || init.AppSecret == "" {
		return nil, fmt.Errorf("bridge needs the existing bot's app ID and secret via notifications.webhook.appIdEnv/appSecretEnv (or FEISHU_APP_ID/FEISHU_APP_SECRET)")
	}
	// A host crash closes stdin, but upstream may still be draining sessions.
	// Ask that live instance to exit rather than unlinking its socket or using
	// a persisted PID. The service lock serializes hosts for this state dir.
	previous := &Client{socket: init.Socket}
	probe, cancel := context.WithTimeout(ctx, time.Second)
	err := previous.Request(probe, http.MethodGet, "/health", nil)
	cancel()
	if err == nil {
		if err := previous.Request(ctx, http.MethodPost, "/shutdown", nil); err != nil {
			return nil, fmt.Errorf("stop previous bridge: %w", err)
		}
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Lstat(init.Socket); os.IsNotExist(err) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				return nil, fmt.Errorf("previous bridge has not finished shutting down")
			case <-tick.C:
			}
		}
	}
	cmd := exec.Command("node", entry)
	for _, variable := range os.Environ() {
		key, value, _ := strings.Cut(variable, "=")
		if value == init.AppSecret || slices.Contains(privateSecrets, value) || key == "FEISHU_APP_SECRET" || key == "LOOPER_BRIDGE_APP_SECRET" {
			continue
		}
		cmd.Env = append(cmd.Env, variable)
	}
	cmd.Env = append(cmd.Env, "LOOPER_BRIDGE_HOME="+home)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = pipe.Close()
		return nil, err
	}
	client := &Client{socket: init.Socket, cmd: cmd, pipe: pipe, done: make(chan struct{})}
	go func() { client.err = cmd.Wait(); close(client.done) }()
	if err := json.NewEncoder(pipe).Encode(init); err != nil {
		_ = client.Close()
		return nil, err
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = client.Close()
			return nil, ctx.Err()
		case <-client.done:
			return nil, fmt.Errorf("bridge exited before readiness: %v; inspect %s", client.err, filepath.Dir(entry))
		case <-deadline.C:
			_ = client.Close()
			return nil, fmt.Errorf("bridge connection did not become ready within 30 seconds")
		case <-tick.C:
			probe, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			err := client.Request(probe, http.MethodGet, "/health", nil)
			cancel()
			if err == nil {
				return client, nil
			}
		}
	}
}

func (c *Client) Request(ctx context.Context, method, path string, input any) error {
	var data []byte
	var err error
	if input != nil {
		data, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, method, "http://bridge"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("bridge HTTP %d: %s", res.StatusCode, body)
	}
	return nil
}

func (c *Client) Done() <-chan struct{} { return c.done }

// Close acts only on the child handle created above. Closing the pipe also
// triggers upstream session cleanup if SIGTERM handling is delayed.
func (c *Client) Close() error {
	c.once.Do(func() {
		_ = c.pipe.Close()
		select {
		case <-c.done:
			return
		default:
		}
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			// This is the live child process group, never a recovered PID.
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			<-c.done
		}
	})
	return c.err
}
