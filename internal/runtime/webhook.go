package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

const webhookListenerPath = "/webhook/forward"

var webhookForwardEvents = []string{"pull_request", "issue_comment", "pull_request_review", "pull_request_review_comment"}

type WebhookStatus struct {
	Enabled                     bool                    `json:"enabled"`
	FallbackPollIntervalSeconds int                     `json:"fallbackPollIntervalSeconds"`
	ListenerPath                string                  `json:"listenerPath"`
	EndpointURL                 string                  `json:"endpointUrl"`
	Degraded                    bool                    `json:"degraded"`
	DegradedReasons             []string                `json:"degradedReasons"`
	Queue                       WebhookQueueStatus      `json:"queue"`
	Counters                    WebhookCounters         `json:"counters"`
	RecentOutcomes              []WebhookRecentOutcome  `json:"recentOutcomes"`
	Forwarders                  []WebhookForwarderState `json:"forwarders"`
}

type WebhookQueueStatus struct {
	Pending       int `json:"pending"`
	Capacity      int `json:"capacity"`
	ActiveWorkers int `json:"activeWorkers"`
}

type WebhookCounters struct {
	DeliveriesReceived int `json:"deliveriesReceived"`
	Coalesced          int `json:"coalesced"`
	Dropped            int `json:"dropped"`
	Queued             int `json:"queued"`
	Processed          int `json:"processed"`
	Failed             int `json:"failed"`
}

type WebhookRecentOutcome struct {
	At      string `json:"at"`
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

type WebhookForwarderState struct {
	Repo          string   `json:"repo"`
	Running       bool     `json:"running"`
	PID           *int     `json:"pid,omitempty"`
	Command       []string `json:"command"`
	RestartCount  int      `json:"restartCount"`
	LastStartedAt *string  `json:"lastStartedAt,omitempty"`
	LastExitAt    *string  `json:"lastExitAt,omitempty"`
	LastError     string   `json:"lastError,omitempty"`
	StdoutTail    []string `json:"stdoutTail,omitempty"`
	StderrTail    []string `json:"stderrTail,omitempty"`
}

type webhookRuntime struct {
	logger  bootstrap.Logger
	now     func() time.Time
	ghPath  string
	status  WebhookStatus
	stopCh  chan struct{}
	mu      sync.RWMutex
	wg      sync.WaitGroup
	stopped bool
}

func newWebhookRuntime(cfg config.Config, logger bootstrap.Logger, now func() time.Time) *webhookRuntime {
	if now == nil {
		now = time.Now
	}
	endpointURL := strings.TrimRight(webhookBaseURL(cfg), "/") + webhookListenerPath
	status := WebhookStatus{
		Enabled:                     cfg.Webhook.Enabled,
		FallbackPollIntervalSeconds: cfg.Webhook.FallbackPollIntervalSeconds,
		ListenerPath:                webhookListenerPath,
		EndpointURL:                 endpointURL,
		DegradedReasons:             []string{},
		Queue:                       WebhookQueueStatus{Capacity: 0},
		RecentOutcomes:              []WebhookRecentOutcome{},
		Forwarders:                  []WebhookForwarderState{},
	}
	rt := &webhookRuntime{logger: logger, now: now, ghPath: strings.TrimSpace(derefString(cfg.Tools.GHPath)), status: status, stopCh: make(chan struct{})}
	if !cfg.Webhook.Enabled {
		return rt
	}
	if !isLoopbackHost(cfg.Server.Host) {
		rt.addDegradedReason("server.host is not loopback; webhook forwarders require a loopback daemon endpoint")
	}
	if rt.ghPath == "" {
		rt.addDegradedReason("gh is not configured or could not be resolved")
	}
	return rt
}

func (w *webhookRuntime) RecordDelivery(eventType, deliveryID string) {
	if w == nil {
		return
	}
	ackAt := formatJavaScriptISOString(w.now().UTC())
	message := strings.TrimSpace(eventType)
	if strings.TrimSpace(deliveryID) != "" {
		message = fmt.Sprintf("%s (%s)", message, strings.TrimSpace(deliveryID))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.Counters.DeliveriesReceived++
	w.status.RecentOutcomes = append(w.status.RecentOutcomes, WebhookRecentOutcome{At: ackAt, Outcome: "acknowledged", Message: message})
	if len(w.status.RecentOutcomes) > 10 {
		w.status.RecentOutcomes = append([]WebhookRecentOutcome{}, w.status.RecentOutcomes[len(w.status.RecentOutcomes)-10:]...)
	}
}

func (w *webhookRuntime) Start(repos *storage.Repositories) {
	if w == nil || !w.status.Enabled {
		return
	}
	if repos == nil || repos.Projects == nil {
		w.addDegradedReason("project repositories are unavailable")
		return
	}
	projects, err := repos.Projects.List(context.Background())
	if err != nil {
		w.addDegradedReason(fmt.Sprintf("list configured projects: %v", err))
		return
	}
	repoSet := map[string]struct{}{}
	for _, project := range projects {
		repo := repoFromProjectMetadata(project.MetadataJSON)
		if repo == "" {
			continue
		}
		repoSet[repo] = struct{}{}
	}
	for repo := range repoSet {
		w.addForwarder(repo)
	}
	if len(repoSet) == 0 {
		w.addDegradedReason("no configured GitHub repos are available for webhook forwarding")
		return
	}
	if w.ghPath == "" || w.status.Degraded {
		return
	}
	for index := range w.Status().Forwarders {
		w.launchForwarder(index)
	}
}

func (w *webhookRuntime) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	close(w.stopCh)
	forwarders := append([]WebhookForwarderState{}, w.status.Forwarders...)
	w.mu.Unlock()
	for _, forwarder := range forwarders {
		if forwarder.PID == nil {
			continue
		}
		if proc, err := osFindProcess(*forwarder.PID); err == nil {
			_ = proc.Kill()
		}
	}
	w.wg.Wait()
}

func (w *webhookRuntime) Status() WebhookStatus {
	if w == nil {
		return WebhookStatus{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	status := w.status
	status.DegradedReasons = append([]string{}, w.status.DegradedReasons...)
	status.RecentOutcomes = append([]WebhookRecentOutcome{}, w.status.RecentOutcomes...)
	status.Forwarders = append([]WebhookForwarderState{}, w.status.Forwarders...)
	for i := range status.Forwarders {
		status.Forwarders[i].Command = append([]string{}, status.Forwarders[i].Command...)
		status.Forwarders[i].StdoutTail = append([]string{}, status.Forwarders[i].StdoutTail...)
		status.Forwarders[i].StderrTail = append([]string{}, status.Forwarders[i].StderrTail...)
	}
	return status
}

func (w *webhookRuntime) addForwarder(repo string) {
	state := WebhookForwarderState{
		Repo:    repo,
		Command: []string{w.ghPath, "webhook", "forward", "--repo", repo, "--events", strings.Join(webhookForwardEvents, ","), "--url", w.status.EndpointURL},
	}
	w.mu.Lock()
	w.status.Forwarders = append(w.status.Forwarders, state)
	w.mu.Unlock()
}

func (w *webhookRuntime) launchForwarder(index int) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runForwarder(index)
	}()
}

func (w *webhookRuntime) runForwarder(index int) {
	backoff := time.Second
	for {
		w.mu.RLock()
		if w.stopped {
			w.mu.RUnlock()
			return
		}
		state := w.status.Forwarders[index]
		w.mu.RUnlock()

		cmd := exec.Command(state.Command[0], state.Command[1:]...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			w.recordForwarderError(index, fmt.Sprintf("attach stdout: %v", err), true)
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			w.recordForwarderError(index, fmt.Sprintf("attach stderr: %v", err), true)
			return
		}
		if err := cmd.Start(); err != nil {
			w.recordForwarderError(index, err.Error(), true)
			if !w.sleep(backoff) {
				return
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}

		startedAt := formatJavaScriptISOString(w.now().UTC())
		pid := cmd.Process.Pid
		w.mu.Lock()
		w.status.Forwarders[index].Running = true
		w.status.Forwarders[index].PID = &pid
		w.status.Forwarders[index].LastStartedAt = &startedAt
		w.status.Forwarders[index].LastError = ""
		w.mu.Unlock()

		var pipes sync.WaitGroup
		pipes.Add(2)
		go func() { defer pipes.Done(); w.captureTail(index, stdout, true) }()
		go func() { defer pipes.Done(); w.captureTail(index, stderr, false) }()
		err = cmd.Wait()
		pipes.Wait()
		exitedAt := formatJavaScriptISOString(w.now().UTC())
		message := ""
		if err != nil {
			message = err.Error()
		}
		w.mu.Lock()
		w.status.Forwarders[index].Running = false
		w.status.Forwarders[index].PID = nil
		w.status.Forwarders[index].LastExitAt = &exitedAt
		w.status.Forwarders[index].LastError = message
		w.status.Forwarders[index].RestartCount++
		w.mu.Unlock()
		if message != "" {
			w.addDegradedReason(fmt.Sprintf("forwarder for %s exited: %s", state.Repo, message))
		}
		if !w.sleep(backoff) {
			return
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (w *webhookRuntime) captureTail(index int, pipe io.ReadCloser, stdout bool) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		w.mu.Lock()
		if stdout {
			w.status.Forwarders[index].StdoutTail = appendTail(w.status.Forwarders[index].StdoutTail, scanner.Text(), 20)
		} else {
			w.status.Forwarders[index].StderrTail = appendTail(w.status.Forwarders[index].StderrTail, scanner.Text(), 20)
		}
		w.mu.Unlock()
	}
}

func (w *webhookRuntime) recordForwarderError(index int, message string, degraded bool) {
	w.mu.Lock()
	w.status.Forwarders[index].LastError = message
	w.mu.Unlock()
	if degraded {
		w.addDegradedReason(fmt.Sprintf("forwarder for %s failed: %s", w.Status().Forwarders[index].Repo, message))
	}
}

func (w *webhookRuntime) addDegradedReason(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, existing := range w.status.DegradedReasons {
		if existing == reason {
			w.status.Degraded = true
			return
		}
	}
	w.status.Degraded = true
	w.status.DegradedReasons = append(w.status.DegradedReasons, reason)
}

func (w *webhookRuntime) sleep(duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-w.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

func webhookBaseURL(cfg config.Config) string {
	return fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port)
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func appendTail(lines []string, line string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	lines = append(lines, line)
	if len(lines) <= limit {
		return lines
	}
	return append([]string{}, lines[len(lines)-limit:]...)
}

var osFindProcess = func(pid int) (*os.Process, error) {
	return os.FindProcess(pid)
}
