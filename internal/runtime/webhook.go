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
	"syscall"
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
	Adopted       bool     `json:"adopted"`
	Latched       bool     `json:"latched"`
	LatchReason   *string  `json:"latchReason,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	SpawnedAt     *string  `json:"spawnedAt,omitempty"`
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
	bootstrapMu     sync.Mutex
	wg              sync.WaitGroup
	stopped         bool
	reconcileRetry  bool
	daemonID        string
	probe           processProbe
	forwarderStore  *storage.WebhookForwardersRepository
	bootstrapDone   bool
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
	rt := &webhookRuntime{logger: logger, now: now, ghPath: strings.TrimSpace(derefString(cfg.Tools.GHPath)), status: status, stopCh: make(chan struct{}), forwarderStopCh: map[string]chan struct{}{}, daemonID: newDaemonID(), probe: defaultProcessProbe{}}
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
	w.Bootstrap(context.Background(), repos)
	w.Reconcile(repos)
}

func (w *webhookRuntime) Bootstrap(ctx context.Context, repos *storage.Repositories) {
	if w == nil || !w.status.Enabled || repos == nil || repos.WebhookForwarders == nil || repos.Projects == nil {
		return
	}
	w.bootstrapMu.Lock()
	defer w.bootstrapMu.Unlock()
	if w.bootstrapCompleted() {
		return
	}
	w.syncForwarderStore(repos.WebhookForwarders)
	records, err := repos.WebhookForwarders.List(ctx)
	if err != nil {
		w.addDegradedReason(fmt.Sprintf("webhook forwarder bootstrap is incomplete: list records: %v", err))
		return
	}
	if len(records) == 0 {
		w.mu.Lock()
		w.bootstrapDone = true
		w.mu.Unlock()
		return
	}
	projects, err := repos.Projects.List(ctx)
	if err != nil {
		w.addDegradedReason(fmt.Sprintf("webhook forwarder bootstrap is incomplete: list projects: %v", err))
		return
	}
	desiredRepos := uniqueConfiguredWebhookRepos(projects)
	desired := map[string]struct{}{}
	for _, repo := range desiredRepos {
		desired[repo] = struct{}{}
	}
	w.clearTransientReconcileDegradedReasons()
	inconclusive := false
	for _, record := range records {
		if _, ok := desired[record.Repo]; !ok {
			w.cleanupForwarderRecord(ctx, record, "repo_removed")
			continue
		}
		if w.forwarderState(record.Repo) != nil {
			continue
		}
		state := WebhookForwarderState{Repo: record.Repo, Command: []string{record.GHPath, "webhook", "forward", "--repo", record.Repo, "--events", record.Events, "--url", record.Endpoint}}
		reason := w.adoptionGate(record, state.Command)
		if reason == "" {
			w.adoptForwarder(record, state.Command)
			continue
		}
		if w.logger != nil {
			w.logger.Warn("webhook.forwarder.adoption_rejected", map[string]any{"repo": record.Repo, "pid": record.PID, "reason": reason})
		}
		if reason == "probe_error" {
			inconclusive = true
			w.addDegradedReason(fmt.Sprintf("webhook forwarder bootstrap is incomplete: adoption probe for %s failed", record.Repo))
			continue
		}
		w.cleanupForwarderRecord(ctx, record, "adoption_rejected")
	}
	if inconclusive {
		return
	}
	w.mu.Lock()
	w.bootstrapDone = true
	w.mu.Unlock()
}

func (w *webhookRuntime) bootstrapCompleted() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.bootstrapDone
}

func (w *webhookRuntime) syncForwarderStore(store *storage.WebhookForwardersRepository) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.forwarderStore = store
}

func (w *webhookRuntime) canLaunchForwarders() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status.Enabled && w.ghPath != "" && !w.hasLaunchBlockingDegradedReasonLocked()
}

func (w *webhookRuntime) Reconcile(repos *storage.Repositories) {
	if w == nil || !w.status.Enabled {
		return
	}
	if repos != nil {
		w.syncForwarderStore(repos.WebhookForwarders)
	}
	if repos != nil && repos.WebhookForwarders != nil && !w.bootstrapCompleted() {
		w.Bootstrap(context.Background(), repos)
		if !w.bootstrapCompleted() {
			w.scheduleReconcileRetry(repos)
			return
		}
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
	return w.hasLaunchBlockingDegradedReasonLocked()
}

func (w *webhookRuntime) hasLaunchBlockingDegradedReasonLocked() bool {
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
	w.mu.Unlock()
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

func (w *webhookRuntime) forwarderState(repo string) *WebhookForwarderState {
	if w == nil {
		return nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i := range w.status.Forwarders {
		if w.status.Forwarders[i].Repo == repo {
			state := w.status.Forwarders[i]
			return &state
		}
	}
	return nil
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
			wantFingerprint, _ := commandFingerprint(w.ghPath, state.Repo, webhookForwardEvents, w.status.EndpointURL)
			if state.Latched && state.Fingerprint != "" && state.Fingerprint != wantFingerprint {
				stops = append(stops, stopRequest{repo: state.Repo, pid: state.PID, stopCh: w.forwarderStopCh[state.Repo]})
				delete(w.forwarderStopCh, state.Repo)
				launchRepos = append(launchRepos, state.Repo)
				kept = append(kept, WebhookForwarderState{Repo: state.Repo, Command: []string{w.ghPath, "webhook", "forward", "--repo", state.Repo, "--events", strings.Join(webhookForwardEvents, ","), "--url", w.status.EndpointURL}, Fingerprint: wantFingerprint})
				w.forwarderStopCh[state.Repo] = make(chan struct{})
				existing[state.Repo] = struct{}{}
				if w.logger != nil {
					w.logger.Info("webhook.forwarder.unlatched", map[string]any{"repo": state.Repo, "trigger": "config_change"})
				}
				continue
			}
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
		fingerprint, _ := commandFingerprint(w.ghPath, repo, webhookForwardEvents, w.status.EndpointURL)
		kept = append(kept, WebhookForwarderState{
			Repo:        repo,
			Command:     []string{w.ghPath, "webhook", "forward", "--repo", repo, "--events", strings.Join(webhookForwardEvents, ","), "--url", w.status.EndpointURL},
			Fingerprint: fingerprint,
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
				_ = proc.Signal(syscall.SIGTERM)
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

func (w *webhookRuntime) adoptionGate(record storage.WebhookForwarderRecord, command []string) string {
	pid := int(record.PID)
	probe := w.processProbe()
	alive, err := probe.IsAlive(pid)
	if err != nil {
		return "probe_error"
	}
	if !alive {
		return "pid_dead"
	}
	start, err := probe.StartTime(pid)
	if err != nil {
		return "probe_error"
	}
	if start != record.ProcessStart {
		return "process_start_mismatch"
	}
	exe, err := probe.ExecutablePath(pid)
	if err != nil {
		return "probe_error"
	}
	argv, err := probe.Argv(pid)
	if err != nil || len(argv) == 0 {
		return "probe_error"
	}
	if exe != record.GHPath && argv[0] != record.GHPath {
		return "gh_path_mismatch"
	}
	ghPath, endpoint, events := w.ghPath, w.status.EndpointURL, webhookForwardEvents
	if ghPath == "" {
		ghPath = record.GHPath
	}
	if endpoint != record.Endpoint || endpoint != w.status.EndpointURL {
		return "endpoint_mismatch"
	}
	fingerprint, _ := commandFingerprint(ghPath, record.Repo, events, endpoint)
	if fingerprint != record.Fingerprint {
		return "fingerprint_mismatch"
	}
	if !argvMatchesWebhookForward(argv, record.Repo, events, endpoint) || strings.Contains(strings.ToLower(strings.Join(argv, " ")), "defunct") {
		return "argv_mismatch"
	}
	return ""
}

func (w *webhookRuntime) adoptForwarder(record storage.WebhookForwarderRecord, command []string) {
	pid := int(record.PID)
	spawnedAt := formatJavaScriptISOString(time.Unix(0, record.SpawnedAt).UTC())
	stopCh := make(chan struct{})
	w.mu.Lock()
	if w.forwarderStopCh == nil {
		w.forwarderStopCh = map[string]chan struct{}{}
	}
	w.forwarderStopCh[record.Repo] = stopCh
	w.status.Forwarders = append(w.status.Forwarders, WebhookForwarderState{Repo: record.Repo, Running: true, PID: &pid, Adopted: true, Fingerprint: record.Fingerprint, SpawnedAt: &spawnedAt, Command: append([]string{}, command...)})
	w.mu.Unlock()
	if w.logger != nil {
		w.logger.Info("webhook.forwarder.adopted", map[string]any{"repo": record.Repo, "pid": pid, "process_start": record.ProcessStart, "fingerprint": record.Fingerprint})
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		proc := &adoptedForwarderProcess{pid: pid, processStart: record.ProcessStart, probe: w.processProbe()}
		done := make(chan error, 1)
		go func() { done <- proc.Wait() }()
		select {
		case <-w.stopCh:
			_ = proc.Stop()
			w.waitForAdoptedStop(proc, record.Repo)
		case <-stopCh:
			_ = proc.Stop()
			w.waitForAdoptedStop(proc, record.Repo)
		case err := <-done:
			exitedAt := formatJavaScriptISOString(w.currentTime().UTC())
			message := ""
			if err != nil {
				message = err.Error()
			}
			w.updateForwarder(record.Repo, stopCh, func(state *WebhookForwarderState) {
				state.Running = false
				state.PID = nil
				state.LastExitAt = &exitedAt
				state.LastError = message
			})
			w.deleteForwarderRecord(record.Repo)
			if !w.isStopped() {
				w.addDegradedReason(fmt.Sprintf("forwarder for %s exited: %s", record.Repo, message))
				if w.canLaunchForwarders() && w.replaceForwarderForRespawn(record.Repo) {
					w.launchForwarder(record.Repo)
				} else {
					w.removeForwarder(record.Repo)
				}
			}
		}
	}()
}

func (w *webhookRuntime) waitForAdoptedStop(proc *adoptedForwarderProcess, repo string) {
	deadline := time.Now().Add(w.shutdownTimeout())
	for time.Now().Before(deadline) {
		alive, same, known := w.originalProcessState(proc.pid, proc.processStart)
		if !known {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !alive || !same {
			w.deleteForwarderRecord(repo)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if alive, same, known := w.originalProcessState(proc.pid, proc.processStart); known && alive && same {
		_ = proc.Kill()
	}
	for deadline := time.Now().Add(w.shutdownTimeout()); time.Now().Before(deadline); {
		alive, same, known := w.originalProcessState(proc.pid, proc.processStart)
		if !known {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !alive || !same {
			w.deleteForwarderRecord(repo)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *webhookRuntime) originalProcessState(pid int, processStart int64) (alive bool, sameIdentity bool, known bool) {
	probe := w.processProbe()
	alive, err := probe.IsAlive(pid)
	if err != nil {
		return false, false, false
	}
	if !alive {
		return false, false, true
	}
	start, err := probe.StartTime(pid)
	if err != nil {
		return true, false, false
	}
	if start != processStart {
		return true, false, true
	}
	return true, true, true
}

func (w *webhookRuntime) cleanupForwarderRecord(ctx context.Context, record storage.WebhookForwarderRecord, reason string) {
	pid := int(record.PID)
	verifyReason := ""
	deleteRow := false
	probe := w.processProbe()
	if alive, err := probe.IsAlive(pid); err != nil {
		verifyReason = "probe_error"
	} else if !alive {
		verifyReason = "pid_dead"
		deleteRow = true
	} else if start, err := probe.StartTime(pid); err != nil {
		verifyReason = "probe_error"
	} else if start != record.ProcessStart {
		verifyReason = "process_start_mismatch"
		deleteRow = true
	} else if exe, err := probe.ExecutablePath(pid); err != nil {
		verifyReason = "probe_error"
	} else if exe != record.GHPath {
		verifyReason = "gh_path_mismatch"
		deleteRow = true
	} else if argv, err := probe.Argv(pid); err != nil {
		verifyReason = "probe_error"
	} else if !argvMatchesWebhookForward(argv, record.Repo, strings.Split(record.Events, ","), record.Endpoint) {
		verifyReason = "argv_mismatch"
		deleteRow = true
	}
	if verifyReason == "gh_path_mismatch" {
		if argv, err := probe.Argv(pid); err == nil && len(argv) > 0 && argv[0] == record.GHPath && argvMatchesWebhookForward(argv, record.Repo, strings.Split(record.Events, ","), record.Endpoint) {
			verifyReason = ""
			deleteRow = false
		}
	}
	if verifyReason != "" {
		if w.logger != nil {
			w.logger.Warn("webhook.forwarder.orphan_refused", map[string]any{"repo": record.Repo, "pid": pid, "reason": verifyReason})
		}
		if !deleteRow {
			return
		}
	} else {
		proc, err := osFindProcess(pid)
		if err != nil {
			if w.logger != nil {
				w.logger.Warn("webhook.forwarder.orphan_refused", map[string]any{"repo": record.Repo, "pid": pid, "reason": "find_process_error"})
			}
			return
		}
		_ = proc.Signal(syscall.SIGTERM)
		deadline := time.Now().Add(w.shutdownTimeout())
		for time.Now().Before(deadline) {
			alive, same, known := w.originalProcessState(pid, record.ProcessStart)
			if !known {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if !alive || !same {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if alive, same, known := w.originalProcessState(pid, record.ProcessStart); known && alive && same {
			_ = proc.Kill()
		}
		for deadline := time.Now().Add(w.shutdownTimeout()); time.Now().Before(deadline); {
			alive, same, known := w.originalProcessState(pid, record.ProcessStart)
			if !known {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if !alive || !same {
				deleteRow = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !deleteRow {
			return
		}
		if w.logger != nil {
			w.logger.Info("webhook.forwarder.orphan_cleaned", map[string]any{"repo": record.Repo, "pid": pid, "reason": reason})
		}
	}
	if w.forwarderStore != nil {
		_ = w.forwarderStore.Delete(ctx, record.Repo)
	}
}

func (w *webhookRuntime) replaceForwarderForRespawn(repo string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	kept := w.status.Forwarders[:0]
	found := false
	for _, state := range w.status.Forwarders {
		if state.Repo == repo {
			found = true
			continue
		}
		kept = append(kept, state)
	}
	if !found {
		return false
	}
	stopCh := make(chan struct{})
	w.forwarderStopCh[repo] = stopCh
	fingerprint, _ := commandFingerprint(w.ghPath, repo, webhookForwardEvents, w.status.EndpointURL)
	w.status.Forwarders = append(kept, WebhookForwarderState{Repo: repo, Command: []string{w.ghPath, "webhook", "forward", "--repo", repo, "--events", strings.Join(webhookForwardEvents, ","), "--url", w.status.EndpointURL}, Fingerprint: fingerprint})
	return true
}

func (w *webhookRuntime) removeForwarder(repo string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.status.Forwarders[:0]
	for _, state := range w.status.Forwarders {
		if state.Repo == repo {
			continue
		}
		kept = append(kept, state)
	}
	w.status.Forwarders = kept
	delete(w.forwarderStopCh, repo)
}

func (w *webhookRuntime) processProbe() processProbe {
	if w == nil || w.probe == nil {
		return defaultProcessProbe{}
	}
	return w.probe
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
		if w.bootstrapCompleted() && w.currentForwarderStore() == nil {
			w.recordForwarderError(repo, stopCh, "webhook forwarder store is unavailable", true)
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
		pid := cmd.Process.Pid
		processStart, err := w.processProbe().StartTime(pid)
		if err != nil {
			w.killAndWait(cmd)
			w.recordForwarderError(repo, stopCh, fmt.Sprintf("read process start: %v", err), true)
			if !w.sleep(backoff, stopCh) {
				return
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}
		if store := w.currentForwarderStore(); store != nil {
			record := webhookForwarderRecordFromState(repo, pid, processStart, state.Command, w.daemonID, w.currentTime())
			if err := store.Upsert(context.Background(), record); err != nil {
				w.killAndWait(cmd)
				w.recordForwarderError(repo, stopCh, fmt.Sprintf("persist forwarder record: %v", err), true)
				if !w.sleep(backoff, stopCh) {
					return
				}
				if backoff < 10*time.Second {
					backoff *= 2
				}
				continue
			}
		} else if w.bootstrapCompleted() {
			w.killAndWait(cmd)
			w.recordForwarderError(repo, stopCh, "webhook forwarder store is unavailable", true)
			return
		}
		if webhookForwarderStartedHook != nil {
			webhookForwarderStartedHook()
		}
		stopKillDone := make(chan struct{})
		go func() {
			select {
			case <-w.stopCh:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(syscall.SIGTERM)
					select {
					case <-stopKillDone:
					case <-time.After(w.shutdownTimeout()):
						_ = cmd.Process.Kill()
					}
				}
			case <-stopCh:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(syscall.SIGTERM)
					select {
					case <-stopKillDone:
					case <-time.After(w.shutdownTimeout()):
						_ = cmd.Process.Kill()
					}
				}
			case <-stopKillDone:
			}
		}()

		startedAt := formatJavaScriptISOString(w.currentTime().UTC())
		fingerprint, _ := commandFingerprint(w.ghPath, repo, webhookForwardEvents, w.status.EndpointURL)
		spawnedAt := startedAt
		w.clearForwarderDegradedReasons(repo)
		w.updateForwarder(repo, stopCh, func(state *WebhookForwarderState) {
			state.Running = true
			state.PID = &pid
			state.Adopted = false
			state.Fingerprint = fingerprint
			state.SpawnedAt = &spawnedAt
			state.LastStartedAt = &startedAt
			state.LastError = ""
			state.StdoutTail = nil
			state.StderrTail = nil
		})
		if w.logger != nil {
			w.logger.Info("webhook.forwarder.spawned", map[string]any{"repo": repo, "pid": pid, "fingerprint": fingerprint, "daemon_id": w.daemonID})
		}

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
		w.deleteForwarderRecord(repo)
		classification := classifyForwarderExit(w.stderrTail(repo, stopCh), err)
		if w.logger != nil {
			w.logger.Info("webhook.forwarder.exited", map[string]any{"repo": repo, "pid": pid, "exit_class": string(classification.Class), "matched_pattern": classification.MatchedPattern, "wait_err": message})
		}
		if classification.Class == forwarderExitTerminal && !w.isStopped() && w.hasForwarder(repo, stopCh) {
			reason := forwarderLatchReason(classification.MatchedPattern)
			w.updateForwarder(repo, stopCh, func(state *WebhookForwarderState) {
				state.Latched = true
				state.LatchReason = &reason
				state.LastError = reason
			})
			w.addDegradedReason(fmt.Sprintf("forwarder for %s latched: %s; polling fallback continues every %d seconds", state.Repo, reason, w.status.FallbackPollIntervalSeconds))
			if w.logger != nil {
				w.logger.Warn("webhook.forwarder.latched", map[string]any{"repo": repo, "latch_reason": reason})
			}
			return
		}
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

func (w *webhookRuntime) currentForwarderStore() *storage.WebhookForwardersRepository {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.forwarderStore
}

func (w *webhookRuntime) deleteForwarderRecord(repo string) {
	if store := w.currentForwarderStore(); store != nil {
		if err := store.Delete(context.Background(), repo); err != nil && w.logger != nil {
			w.logger.Warn("delete webhook forwarder record failed", map[string]any{"repo": repo, "error": err.Error()})
		}
	}
}

func (w *webhookRuntime) stderrTail(repo string, stopCh chan struct{}) []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for _, state := range w.status.Forwarders {
		if state.Repo == repo && sameStopChannel(w.forwarderStopCh[repo], stopCh) {
			return append([]string{}, state.StderrTail...)
		}
	}
	return nil
}

func forwarderLatchReason(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = "terminal gh webhook forward failure"
	}
	return fmt.Sprintf("terminal gh webhook forward failure matched %q; remove the conflicting GitHub webhook or fix gh authentication, then restart looperd", pattern)
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
		return reason == "project repositories are unavailable" || strings.HasPrefix(reason, "list configured projects: ") || strings.HasPrefix(reason, "webhook forwarder bootstrap is incomplete")
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

func (w *webhookRuntime) killAndWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func (w *webhookRuntime) shutdownTimeout() time.Duration {
	return 5 * time.Second
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
