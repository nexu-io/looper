package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

const webhookListenerPath = "/webhook/forward"

const noConfiguredWebhookReposReason = "no configured GitHub repos are available for webhook forwarding"

var webhookReconcileRetryDelay = 5 * time.Second

var webhookForwardEvents = []string{"pull_request", "issue_comment", "pull_request_review", "pull_request_review_comment", "push", "check_run"}

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
	logger          bootstrap.Logger
	now             func() time.Time
	ghPath          string
	status          WebhookStatus
	stopCh          chan struct{}
	forwarderStopCh map[string]chan struct{}
	mu              sync.RWMutex
	wg              sync.WaitGroup
	stopped         bool
	reconcileRetry  bool
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
	rt := &webhookRuntime{logger: logger, now: now, ghPath: strings.TrimSpace(derefString(cfg.Tools.GHPath)), status: status, stopCh: make(chan struct{}), forwarderStopCh: map[string]chan struct{}{}}
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
	ackAt := formatJavaScriptISOString(w.currentTime().UTC())
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
	w.Reconcile(repos)
}

func (w *webhookRuntime) Reconcile(repos *storage.Repositories) {
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
		w.scheduleReconcileRetry(repos)
		return
	}
	w.clearTransientReconcileDegradedReasons()
	repoSet := map[string]struct{}{}
	for _, project := range projects {
		repo := repoFromProjectMetadata(project.MetadataJSON)
		if repo == "" {
			continue
		}
		repoSet[repo] = struct{}{}
	}
	launchRepos := w.reconcileForwarders(repoSet)
	if len(repoSet) == 0 {
		w.addDegradedReason(noConfiguredWebhookReposReason)
		return
	}
	w.clearDegradedReasons(func(reason string) bool {
		return reason == noConfiguredWebhookReposReason
	})
	if w.ghPath == "" || w.hasLaunchBlockingDegradedReason() {
		return
	}
	for _, repo := range launchRepos {
		w.launchForwarder(repo)
	}
}

func (w *webhookRuntime) hasLaunchBlockingDegradedReason() bool {
	if w == nil {
		return false
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, reason := range w.status.DegradedReasons {
		if !strings.HasPrefix(reason, "forwarder for ") {
			return true
		}
	}
	return false
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

func (w *webhookRuntime) reconcileForwarders(repoSet map[string]struct{}) []string {
	launchRepos := []string{}
	type stopRequest struct {
		repo   string
		pid    *int
		stopCh chan struct{}
	}
	stops := []stopRequest{}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.forwarderStopCh == nil {
		w.forwarderStopCh = map[string]chan struct{}{}
	}
	kept := make([]WebhookForwarderState, 0, len(repoSet))
	existing := make(map[string]struct{}, len(w.status.Forwarders))
	for _, state := range w.status.Forwarders {
		if _, ok := repoSet[state.Repo]; ok {
			kept = append(kept, state)
			existing[state.Repo] = struct{}{}
			continue
		}
		stops = append(stops, stopRequest{repo: state.Repo, pid: state.PID, stopCh: w.forwarderStopCh[state.Repo]})
		delete(w.forwarderStopCh, state.Repo)
	}
	for repo := range repoSet {
		if _, ok := existing[repo]; ok {
			continue
		}
		kept = append(kept, WebhookForwarderState{
			Repo:    repo,
			Command: []string{w.ghPath, "webhook", "forward", "--repo", repo, "--events", strings.Join(webhookForwardEvents, ","), "--url", w.status.EndpointURL},
		})
		if _, ok := w.forwarderStopCh[repo]; !ok {
			w.forwarderStopCh[repo] = make(chan struct{})
		}
		launchRepos = append(launchRepos, repo)
	}
	w.status.Forwarders = kept
	for _, stop := range stops {
		if stop.stopCh != nil {
			close(stop.stopCh)
		}
		if stop.pid != nil {
			if proc, err := osFindProcess(*stop.pid); err == nil {
				_ = proc.Kill()
			}
		}
	}
	for _, stop := range stops {
		prefix := fmt.Sprintf("forwarder for %s ", strings.TrimSpace(stop.repo))
		filtered := w.status.DegradedReasons[:0]
		for _, reason := range w.status.DegradedReasons {
			if !strings.HasPrefix(reason, prefix) {
				filtered = append(filtered, reason)
			}
		}
		w.status.DegradedReasons = filtered
	}
	w.status.Degraded = len(w.status.DegradedReasons) > 0
	return launchRepos
}

func (w *webhookRuntime) launchForwarder(repo string) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.runForwarder(repo)
	}()
}

func (w *webhookRuntime) isStopped() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.stopped
}

func (w *webhookRuntime) runForwarder(repo string) {
	backoff := time.Second
	for {
		state, stopCh, ok := w.forwarderSnapshot(repo)
		if !ok {
			return
		}

		cmd := execCommand(state.Command[0], state.Command[1:]...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			w.recordForwarderError(repo, stopCh, fmt.Sprintf("attach stdout: %v", err), true)
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			w.recordForwarderError(repo, stopCh, fmt.Sprintf("attach stderr: %v", err), true)
			return
		}
		if err := cmd.Start(); err != nil {
			w.recordForwarderError(repo, stopCh, err.Error(), true)
			if !w.sleep(backoff, stopCh) {
				return
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}
		if webhookForwarderStartedHook != nil {
			webhookForwarderStartedHook()
		}
		stopKillDone := make(chan struct{})
		go func() {
			select {
			case <-w.stopCh:
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			case <-stopCh:
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			case <-stopKillDone:
			}
		}()

		startedAt := formatJavaScriptISOString(w.currentTime().UTC())
		pid := cmd.Process.Pid
		w.clearForwarderDegradedReasons(repo)
		w.updateForwarder(repo, stopCh, func(state *WebhookForwarderState) {
			state.Running = true
			state.PID = &pid
			state.LastStartedAt = &startedAt
			state.LastError = ""
		})

		var pipes sync.WaitGroup
		pipes.Add(2)
		go func() { defer pipes.Done(); w.captureTail(repo, stopCh, stdout, true) }()
		go func() { defer pipes.Done(); w.captureTail(repo, stopCh, stderr, false) }()
		err = cmd.Wait()
		close(stopKillDone)
		pipes.Wait()
		exitedAt := formatJavaScriptISOString(w.currentTime().UTC())
		message := ""
		if err != nil {
			message = err.Error()
		}
		w.updateForwarder(repo, stopCh, func(state *WebhookForwarderState) {
			state.Running = false
			state.PID = nil
			state.LastExitAt = &exitedAt
			state.LastError = message
			state.RestartCount++
		})
		if message != "" && !w.isStopped() && w.hasForwarder(repo, stopCh) {
			w.addDegradedReason(fmt.Sprintf("forwarder for %s exited: %s", state.Repo, message))
		}
		if !w.sleep(backoff, stopCh) {
			return
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (w *webhookRuntime) captureTail(repo string, stopCh chan struct{}, pipe io.ReadCloser, stdout bool) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		w.updateForwarder(repo, stopCh, func(state *WebhookForwarderState) {
			if stdout {
				state.StdoutTail = appendTail(state.StdoutTail, scanner.Text(), 20)
			} else {
				state.StderrTail = appendTail(state.StderrTail, scanner.Text(), 20)
			}
		})
	}
}

func (w *webhookRuntime) recordForwarderError(repo string, stopCh chan struct{}, message string, degraded bool) {
	w.updateForwarder(repo, stopCh, func(state *WebhookForwarderState) {
		state.LastError = message
	})
	if degraded && w.hasForwarder(repo, stopCh) {
		w.addDegradedReason(fmt.Sprintf("forwarder for %s failed: %s", strings.TrimSpace(repo), message))
	}
}

func (w *webhookRuntime) forwarderSnapshot(repo string) (WebhookForwarderState, chan struct{}, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.stopped {
		return WebhookForwarderState{}, nil, false
	}
	for _, state := range w.status.Forwarders {
		if state.Repo == repo {
			return state, w.forwarderStopCh[repo], true
		}
	}
	return WebhookForwarderState{}, nil, false
}

func (w *webhookRuntime) updateForwarder(repo string, stopCh chan struct{}, apply func(*WebhookForwarderState)) {
	if apply == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for index := range w.status.Forwarders {
		if w.status.Forwarders[index].Repo == repo && sameStopChannel(w.forwarderStopCh[repo], stopCh) {
			apply(&w.status.Forwarders[index])
			return
		}
	}
}

func (w *webhookRuntime) hasForwarder(repo string, stopCh chan struct{}) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, state := range w.status.Forwarders {
		if state.Repo == repo && sameStopChannel(w.forwarderStopCh[repo], stopCh) {
			return true
		}
	}
	return false
}

func sameStopChannel(current, expected chan struct{}) bool {
	return current == expected
}

func (w *webhookRuntime) clearForwarderDegradedReasons(repo string) {
	prefix := fmt.Sprintf("forwarder for %s ", strings.TrimSpace(repo))
	w.clearDegradedReasons(func(reason string) bool {
		return strings.HasPrefix(reason, prefix)
	})
}

func (w *webhookRuntime) clearTransientReconcileDegradedReasons() {
	w.clearDegradedReasons(func(reason string) bool {
		return reason == "project repositories are unavailable" || strings.HasPrefix(reason, "list configured projects: ")
	})
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

func (w *webhookRuntime) clearDegradedReasons(match func(string) bool) {
	if match == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	filtered := make([]string, 0, len(w.status.DegradedReasons))
	for _, reason := range w.status.DegradedReasons {
		if !match(reason) {
			filtered = append(filtered, reason)
		}
	}
	w.status.DegradedReasons = filtered
	w.status.Degraded = len(w.status.DegradedReasons) > 0
}

func (w *webhookRuntime) sleep(duration time.Duration, forwarderStopCh <-chan struct{}) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-w.stopCh:
		return false
	case <-forwarderStopCh:
		return false
	case <-timer.C:
		return true
	}
}

func (w *webhookRuntime) currentTime() time.Time {
	if w == nil || w.now == nil {
		return time.Now()
	}
	return w.now()
}

func (w *webhookRuntime) scheduleReconcileRetry(repos *storage.Repositories) {
	if w == nil || repos == nil {
		return
	}
	w.mu.Lock()
	if w.stopped || w.reconcileRetry {
		w.mu.Unlock()
		return
	}
	w.reconcileRetry = true
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		timer := time.NewTimer(webhookReconcileRetryDelay)
		defer timer.Stop()
		select {
		case <-w.stopCh:
			w.mu.Lock()
			w.reconcileRetry = false
			w.mu.Unlock()
			return
		case <-timer.C:
		}
		w.mu.Lock()
		if w.stopped {
			w.reconcileRetry = false
			w.mu.Unlock()
			return
		}
		w.reconcileRetry = false
		w.mu.Unlock()
		w.Reconcile(repos)
	}()
}

func webhookBaseURL(cfg config.Config) string {
	return "http://" + net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
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

var execCommand = exec.Command

var webhookForwarderStartedHook func()

var osFindProcess = func(pid int) (*os.Process, error) {
	return os.FindProcess(pid)
}
