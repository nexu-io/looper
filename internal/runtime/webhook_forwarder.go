package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

const webhookForwardPath = "/webhook/forward"
const webhookForwarderTailLimit = 20

var webhookForwarderEvents = []string{
	"pull_request",
	"issue_comment",
	"pull_request_review",
	"pull_request_review_comment",
}

type WebhookRuntimeStatus struct {
	Enabled         bool                     `json:"enabled"`
	Healthy         bool                     `json:"healthy"`
	Degraded        bool                     `json:"degraded"`
	Endpoint        *string                  `json:"endpoint,omitempty"`
	DegradedReasons []string                 `json:"degradedReasons,omitempty"`
	Forwarders      []WebhookForwarderStatus `json:"forwarders"`
}

type WebhookForwarderStatus struct {
	Repo         string   `json:"repo"`
	Events       []string `json:"events"`
	Command      []string `json:"command"`
	Running      bool     `json:"running"`
	RespawnCount int      `json:"respawnCount"`
	LastStartAt  *string  `json:"lastStartAt,omitempty"`
	LastExitAt   *string  `json:"lastExitAt,omitempty"`
	LastError    *string  `json:"lastError,omitempty"`
	Tail         []string `json:"tail"`
}

type webhookForwarderProcess interface {
	Wait() error
	Stop() error
	Kill() error
}

type webhookForwarderStartResult struct {
	process webhookForwarderProcess
	stdout  io.ReadCloser
	stderr  io.ReadCloser
}

type webhookForwarderStarter func(context.Context, webhookForwarderCommand) (webhookForwarderStartResult, error)

type webhookForwarderCommand struct {
	Path   string
	Repo   string
	URL    string
	Events []string
}

type webhookForwarderManagerOptions struct {
	Config       config.Config
	Logger       bootstrap.Logger
	Now          func() time.Time
	StartProcess webhookForwarderStarter
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	TailLimit    int
	Sleep        func(context.Context, time.Duration) error
	StopTimeout  time.Duration
}

type webhookForwarderManager struct {
	config       config.Config
	logger       bootstrap.Logger
	now          func() time.Time
	startProcess webhookForwarderStarter
	backoffBase  time.Duration
	backoffMax   time.Duration
	tailLimit    int
	sleep        func(context.Context, time.Duration) error
	stopTimeout  time.Duration

	syncMu         sync.Mutex
	mu             sync.RWMutex
	endpoint       *string
	initialReasons []string
	forwarders     map[string]*managedWebhookForwarder
}

type managedWebhookForwarder struct {
	mu      sync.Mutex
	repo    string
	stopCh  chan struct{}
	doneCh  chan struct{}
	status  WebhookForwarderStatus
	process webhookForwarderProcess
}

func newWebhookForwarderManager(options webhookForwarderManagerOptions) *webhookForwarderManager {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	startProcess := options.StartProcess
	if startProcess == nil {
		startProcess = startWebhookForwarderProcess
	}
	backoffBase := options.BackoffBase
	if backoffBase <= 0 {
		backoffBase = time.Second
	}
	backoffMax := options.BackoffMax
	if backoffMax <= 0 {
		backoffMax = 30 * time.Second
	}
	tailLimit := options.TailLimit
	if tailLimit <= 0 {
		tailLimit = webhookForwarderTailLimit
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	stopTimeout := options.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = 5 * time.Second
	}

	manager := &webhookForwarderManager{
		config:       options.Config,
		logger:       options.Logger,
		now:          now,
		startProcess: startProcess,
		backoffBase:  backoffBase,
		backoffMax:   backoffMax,
		tailLimit:    tailLimit,
		sleep:        sleep,
		stopTimeout:  stopTimeout,
		forwarders:   map[string]*managedWebhookForwarder{},
	}
	manager.endpoint, manager.initialReasons = webhookForwardEndpoint(options.Config)
	return manager
}

func (m *webhookForwarderManager) Enabled() bool {
	return m != nil && m.config.Webhook.Enabled
}

func (m *webhookForwarderManager) Sync(ctx context.Context, projects []storage.ProjectRecord) {
	if m == nil {
		return
	}
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	desiredRepos := uniqueConfiguredWebhookRepos(projects)
	if !m.canRun() {
		m.stopAllForwarders()
		return
	}

	desired := make(map[string]struct{}, len(desiredRepos))
	for _, repo := range desiredRepos {
		desired[repo] = struct{}{}
	}

	removed := make([]*managedWebhookForwarder, 0)
	m.mu.Lock()
	for repo, forwarder := range m.forwarders {
		if _, ok := desired[repo]; ok {
			continue
		}
		delete(m.forwarders, repo)
		removed = append(removed, forwarder)
	}
	m.mu.Unlock()
	for _, forwarder := range removed {
		m.stopForwarder(forwarder)
	}
	m.mu.Lock()
	for _, repo := range desiredRepos {
		if _, ok := m.forwarders[repo]; ok {
			continue
		}
		forwarder := &managedWebhookForwarder{
			repo:   repo,
			stopCh: make(chan struct{}),
			doneCh: make(chan struct{}),
			status: WebhookForwarderStatus{
				Repo:    repo,
				Events:  append([]string{}, webhookForwarderEvents...),
				Command: m.commandForRepo(repo),
				Tail:    []string{},
			},
		}
		m.forwarders[repo] = forwarder
		go m.superviseForwarder(ctx, forwarder)
	}
	m.mu.Unlock()
}

func (m *webhookForwarderManager) Stop() {
	if m == nil {
		return
	}
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.stopAllForwarders()
}

func (m *webhookForwarderManager) Status() WebhookRuntimeStatus {
	if m == nil {
		return WebhookRuntimeStatus{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := WebhookRuntimeStatus{
		Enabled:         m.config.Webhook.Enabled,
		Healthy:         true,
		Forwarders:      make([]WebhookForwarderStatus, 0, len(m.forwarders)),
		DegradedReasons: append([]string{}, m.initialReasons...),
	}
	if m.endpoint != nil {
		endpoint := *m.endpoint
		status.Endpoint = &endpoint
	}
	for _, repo := range sortedManagedForwarderRepos(m.forwarders) {
		forwarder := m.forwarders[repo]
		copied := forwarder.status
		copied.Events = append([]string{}, copied.Events...)
		copied.Command = append([]string{}, copied.Command...)
		copied.Tail = append([]string{}, copied.Tail...)
		status.Forwarders = append(status.Forwarders, copied)
		if copied.LastError != nil && strings.TrimSpace(*copied.LastError) != "" {
			status.DegradedReasons = append(status.DegradedReasons, fmt.Sprintf("repo %s: %s", copied.Repo, *copied.LastError))
		}
	}
	if len(status.DegradedReasons) > 0 {
		status.Degraded = true
		status.Healthy = false
	}
	if !status.Enabled {
		status.Healthy = true
		status.Degraded = false
		status.DegradedReasons = nil
	}
	return status
}

func (m *webhookForwarderManager) canRun() bool {
	return m != nil && m.config.Webhook.Enabled && m.endpoint != nil && len(m.initialReasons) == 0
}

func (m *webhookForwarderManager) commandForRepo(repo string) []string {
	if m == nil {
		return nil
	}
	endpoint := ""
	if m.endpoint != nil {
		endpoint = *m.endpoint
	}
	return []string{
		strings.TrimSpace(derefString(m.config.Tools.GHPath)),
		"webhook",
		"forward",
		"--repo=" + repo,
		"--events=" + strings.Join(webhookForwarderEvents, ","),
		"--url=" + endpoint,
	}
}

func (m *webhookForwarderManager) superviseForwarder(ctx context.Context, forwarder *managedWebhookForwarder) {
	defer close(forwarder.doneCh)
	backoff := m.backoffBase
	resetBackoffAfterStart := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-forwarder.stopCh:
			return
		default:
		}

		endpoint := ""
		if m.endpoint != nil {
			endpoint = *m.endpoint
		}
		startResult, err := m.startProcess(ctx, webhookForwarderCommand{
			Path:   strings.TrimSpace(derefString(m.config.Tools.GHPath)),
			Repo:   forwarder.repo,
			URL:    endpoint,
			Events: webhookForwarderEvents,
		})
		if err != nil {
			m.markForwarderError(forwarder, fmt.Sprintf("start failed: %s", err.Error()), false)
			if sleepErr := m.sleepUntilNextAttempt(ctx, forwarder, backoff); sleepErr != nil {
				return
			}
			backoff = boundedWebhookBackoff(backoff, m.backoffBase, m.backoffMax)
			resetBackoffAfterStart = true
			continue
		}
		select {
		case <-ctx.Done():
			_ = startResult.process.Kill()
			if startResult.stdout != nil {
				_ = startResult.stdout.Close()
			}
			if startResult.stderr != nil {
				_ = startResult.stderr.Close()
			}
			_ = startResult.process.Wait()
			return
		case <-forwarder.stopCh:
			_ = startResult.process.Kill()
			if startResult.stdout != nil {
				_ = startResult.stdout.Close()
			}
			if startResult.stderr != nil {
				_ = startResult.stderr.Close()
			}
			_ = startResult.process.Wait()
			return
		default:
		}

		startedAt := formatJavaScriptISOString(m.now().UTC())
		m.mu.Lock()
		forwarder.setProcess(startResult.process)
		forwarder.status.Running = true
		forwarder.status.LastStartAt = &startedAt
		forwarder.status.LastError = nil
		m.mu.Unlock()
		if m.logger != nil {
			m.logger.Info("gh webhook forwarder started", map[string]any{"repo": forwarder.repo, "endpoint": endpoint})
		}
		if resetBackoffAfterStart {
			backoff = m.backoffBase
			resetBackoffAfterStart = false
		}

		var readers sync.WaitGroup
		readers.Add(2)
		go m.captureForwarderOutput(forwarder, "stdout", startResult.stdout, &readers)
		go m.captureForwarderOutput(forwarder, "stderr", startResult.stderr, &readers)
		waitErr := startResult.process.Wait()
		readers.Wait()

		m.mu.Lock()
		forwarder.setProcess(nil)
		forwarder.status.Running = false
		exitedAt := formatJavaScriptISOString(m.now().UTC())
		forwarder.status.LastExitAt = &exitedAt
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-forwarder.stopCh:
			return
		default:
		}

		errText := "process exited"
		if waitErr != nil {
			errText = waitErr.Error()
		}
		m.markForwarderError(forwarder, errText, true)
		if sleepErr := m.sleepUntilNextAttempt(ctx, forwarder, backoff); sleepErr != nil {
			return
		}
		backoff = boundedWebhookBackoff(backoff, m.backoffBase, m.backoffMax)
	}
}

func (m *webhookForwarderManager) sleepUntilNextAttempt(ctx context.Context, forwarder *managedWebhookForwarder, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	select {
	case <-forwarder.stopCh:
		return context.Canceled
	default:
	}
	done := make(chan error, 1)
	go func() {
		done <- m.sleep(ctx, delay)
	}()
	select {
	case <-forwarder.stopCh:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (m *webhookForwarderManager) captureForwarderOutput(forwarder *managedWebhookForwarder, stream string, reader io.ReadCloser, readers *sync.WaitGroup) {
	defer readers.Done()
	if reader == nil {
		return
	}
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		m.recordForwarderOutput(forwarder, stream, line)
	}
	if err := scanner.Err(); err != nil && m.logger != nil {
		m.logger.Warn("gh webhook forwarder output read failed", map[string]any{"repo": forwarder.repo, "stream": stream, "error": err.Error()})
	}
}

func (m *webhookForwarderManager) recordForwarderOutput(forwarder *managedWebhookForwarder, stream string, line string) {
	entry := stream + ": " + line
	m.mu.Lock()
	forwarder.status.Tail = appendTail(forwarder.status.Tail, entry, m.tailLimit)
	m.mu.Unlock()
	if m.logger == nil {
		return
	}
	context := map[string]any{"repo": forwarder.repo, "stream": stream, "line": line}
	if stream == "stderr" {
		m.logger.Warn("gh webhook forwarder output", context)
		return
	}
	m.logger.Info("gh webhook forwarder output", context)
}

func (m *webhookForwarderManager) markForwarderError(forwarder *managedWebhookForwarder, message string, respawn bool) {
	errText := strings.TrimSpace(message)
	if errText == "" {
		errText = "forwarder failed"
	}
	m.mu.Lock()
	forwarder.status.Running = false
	forwarder.status.LastError = &errText
	if respawn {
		forwarder.status.RespawnCount++
	}
	m.mu.Unlock()
	if m.logger != nil {
		m.logger.Warn("gh webhook forwarder degraded", map[string]any{"repo": forwarder.repo, "error": errText, "respawn": respawn})
	}
}

func (m *webhookForwarderManager) stopAllForwarders() {
	m.mu.Lock()
	forwarders := make([]*managedWebhookForwarder, 0, len(m.forwarders))
	for repo, forwarder := range m.forwarders {
		delete(m.forwarders, repo)
		forwarders = append(forwarders, forwarder)
	}
	m.mu.Unlock()
	for _, forwarder := range forwarders {
		m.stopForwarder(forwarder)
	}
}

func (m *webhookForwarderManager) stopForwarder(forwarder *managedWebhookForwarder) {
	if forwarder == nil {
		return
	}
	select {
	case <-forwarder.stopCh:
	default:
		close(forwarder.stopCh)
	}
	if process := forwarder.currentProcess(); process != nil {
		_ = process.Stop()
	}
	if m.waitForForwarderDone(forwarder, m.stopTimeout) {
		return
	}
	if process := forwarder.currentProcess(); process != nil {
		_ = process.Kill()
	}
	_ = m.waitForForwarderDone(forwarder, m.stopTimeout)
}

func (m *webhookForwarderManager) waitForForwarderDone(forwarder *managedWebhookForwarder, timeout time.Duration) bool {
	if timeout <= 0 {
		<-forwarder.doneCh
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-forwarder.doneCh:
		return true
	case <-timer.C:
		return false
	}
}

func uniqueConfiguredWebhookRepos(projects []storage.ProjectRecord) []string {
	seen := map[string]struct{}{}
	repos := make([]string, 0, len(projects))
	for _, project := range projects {
		if project.Archived {
			continue
		}
		repo := repoFromProjectMetadata(project.MetadataJSON)
		if repo == "" {
			continue
		}
		if _, ok := seen[repo]; ok {
			continue
		}
		seen[repo] = struct{}{}
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

func webhookForwardEndpoint(cfg config.Config) (*string, []string) {
	if !cfg.Webhook.Enabled {
		return nil, nil
	}
	reasons := make([]string, 0)
	ghPath := strings.TrimSpace(derefString(cfg.Tools.GHPath))
	if ghPath == "" {
		reasons = append(reasons, "configured/resolved gh executable is missing")
	}
	if !isLoopbackHost(cfg.Server.Host) {
		reasons = append(reasons, fmt.Sprintf("server host %q is not loopback", cfg.Server.Host))
	}
	if len(reasons) > 0 {
		return nil, reasons
	}
	if cfg.Server.BaseURL != nil && strings.TrimSpace(*cfg.Server.BaseURL) != "" {
		base, err := url.Parse(strings.TrimSpace(*cfg.Server.BaseURL))
		if err != nil {
			return nil, []string{fmt.Sprintf("server.baseUrl is invalid: %s", err.Error())}
		}
		if !isLoopbackHost(base.Hostname()) {
			return nil, []string{fmt.Sprintf("server.baseUrl host %q is not loopback", base.Hostname())}
		}
		resolved := base.ResolveReference(&url.URL{Path: webhookForwardPath})
		endpoint := resolved.String()
		return &endpoint, nil
	}
	endpoint := (&url.URL{Scheme: "http", Host: net.JoinHostPort(cfg.Server.Host, fmt.Sprintf("%d", cfg.Server.Port)), Path: webhookForwardPath}).String()
	return &endpoint, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func appendTail(lines []string, line string, limit int) []string {
	lines = append(lines, line)
	if limit > 0 && len(lines) > limit {
		return append([]string{}, lines[len(lines)-limit:]...)
	}
	return lines
}

func boundedWebhookBackoff(current, base, max time.Duration) time.Duration {
	if current <= 0 {
		current = base
	}
	next := current * 2
	if next < base {
		next = base
	}
	if next > max {
		return max
	}
	return next
}

func sortedManagedForwarderRepos(forwarders map[string]*managedWebhookForwarder) []string {
	repos := make([]string, 0, len(forwarders))
	for repo := range forwarders {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type execWebhookForwarderProcess struct {
	cmd *exec.Cmd
}

func (p *execWebhookForwarderProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *execWebhookForwarderProcess) Stop() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	return nil
}

func (p *execWebhookForwarderProcess) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.cmd.Process.Kill(); err != nil {
		return err
	}
	return nil
}

func (f *managedWebhookForwarder) currentProcess() webhookForwarderProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.process
}

func (f *managedWebhookForwarder) setProcess(process webhookForwarderProcess) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.process = process
}

func startWebhookForwarderProcess(ctx context.Context, command webhookForwarderCommand) (webhookForwarderStartResult, error) {
	args := []string{
		"webhook",
		"forward",
		"--repo=" + command.Repo,
		"--events=" + strings.Join(command.Events, ","),
		"--url=" + command.URL,
	}
	cmd := exec.CommandContext(ctx, command.Path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return webhookForwarderStartResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closeWebhookForwarderPipes(stdout, nil)
		return webhookForwarderStartResult{}, err
	}
	if err := cmd.Start(); err != nil {
		closeWebhookForwarderPipes(stdout, stderr)
		return webhookForwarderStartResult{}, err
	}
	return webhookForwarderStartResult{
		process: &execWebhookForwarderProcess{cmd: cmd},
		stdout:  stdout,
		stderr:  stderr,
	}, nil
}

func closeWebhookForwarderPipes(closers ...io.Closer) {
	for _, closer := range closers {
		if closer != nil {
			_ = closer.Close()
		}
	}
}
