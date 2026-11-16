package collab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/executionhost"
	"github.com/nexu-io/looper/internal/feishubridge"
	"github.com/nexu-io/looper/internal/forge"
	gitinfra "github.com/nexu-io/looper/internal/infra/git"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/planestrict"
	"github.com/nexu-io/looper/internal/projects"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
	"github.com/nexu-io/looper/internal/workerhost"
)

// Outcome is the agent's task conclusion. Infrastructure never infers a PR
// requirement from a task title or from the absence of commits.
type Outcome struct {
	State        string   `json:"state"`
	Summary      string   `json:"summary"`
	PullRequests []string `json:"pullRequests,omitempty"`
	WaitingFor   []string `json:"waitingFor,omitempty"`
}

type bridgeTask struct {
	RootID    string  `json:"rootId,omitempty"`
	ThreadID  string  `json:"threadId,omitempty"`
	SessionID string  `json:"sessionId,omitempty"`
	Outcome   Outcome `json:"outcome"`
	Signal    string  `json:"signal,omitempty"`
}

type bridgeHost struct {
	options Options
	cfg     config.Config
	dir     string
	db      *sql.DB
	repos   *storage.Repositories
	chats   *taskChatManager
	notify  *notify.Gateway
	client  *feishubridge.Client
	gh      *githubinfra.Gateway
	mu      sync.Mutex // Serialize local task patches, never hold during Agent execution.
	active  map[string]bool
	ready   atomic.Bool
	network *networkclient.Manager
	sources []planeSource
}

func serveBridge(parent context.Context, options Options) error {
	cfg := privateChatConfig(options.Config)
	dir, err := filepath.Abs(StateDir(cfg, options.StateDir))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := executionhost.AcquireServiceLock(filepath.Join(dir, "service.lock"), "collab", time.Now())
	if err != nil {
		return err
	}
	defer lock.Release()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, filepath.Join(dir, "looper.sqlite"), storage.SQLiteCoordinatorOptions{Migrations: storage.EmbeddedMigrations, BackupDir: filepath.Join(dir, "backups")})
	if err != nil {
		return err
	}
	defer coordinator.Close()
	if _, err := coordinator.MigrationRunner().RunPending(ctx, storage.RunPendingOptions{}); err != nil {
		return err
	}
	repos := storage.NewRepositories(coordinator.DB())
	git := gitinfra.New(gitinfra.Options{GitPath: value(cfg.Tools.GitPath), Repos: repos})
	projectService := projects.Service{DB: coordinator.DB(), Repos: repos, Config: cfg, DetectRepo: func(ctx context.Context, path string) (projects.DetectedRepo, error) {
		repo, err := git.DetectGitHubRepo(ctx, path)
		return projects.DetectedRepo{Repo: repo, Provider: "github"}, err
	}}
	if err := projectService.SyncConfigured(ctx, cfg, time.Now()); err != nil {
		return err
	}
	host := &bridgeHost{options: options, cfg: cfg, dir: dir, db: coordinator.DB(), repos: repos,
		chats: newTaskChatManager(cfg, repos, dir), active: make(map[string]bool),
		gh: githubinfra.New(githubinfra.Options{GHPath: value(cfg.Tools.GHPath), GitPath: value(cfg.Tools.GitPath)})}
	if host.chats == nil {
		return fmt.Errorf("configure the Feishu bot and owner before starting collaboration")
	}
	host.notify = notify.NewGateway(notify.Options{Config: cfg.Notifications, Repositories: repos,
		FeishuCLI: workerhost.PrivateFeishuClient(&cfg), LogFilePath: LogPath(dir),
		ResolveOwnerOpenID: func(project string) string { return config.ProjectOwner(cfg, project) }})
	workspace := filepath.Join(dir, "workspace")
	if err := host.writeWorkspace(workspace); err != nil {
		return err
	}
	_ = os.Remove(SocketPath(dir))
	listener, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(SocketPath(dir), 0600); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /bridge/events", host.turnEvent)
	mux.HandleFunc("GET /status", host.status)
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		var input worker.EnqueueInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		item, err := host.enqueue(r.Context(), input)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, taskStatus(item))
	})
	stopped := make(chan struct{})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		cancel()
		select {
		case <-stopped:
			writeJSON(w, map[string]string{"status": "stopped"})
		case <-r.Context().Done():
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer server.Close()
	log, err := os.OpenFile(filepath.Join(dir, "bridge.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	appID, appSecret := os.Getenv(cfg.Notifications.Webhook.AppIDEnv), os.Getenv(cfg.Notifications.Webhook.AppSecretEnv)
	if appID == "" {
		appID = os.Getenv("FEISHU_APP_ID")
	}
	if appSecret == "" {
		appSecret = os.Getenv("FEISHU_APP_SECRET")
	}
	host.client, err = feishubridge.Start(ctx, options.BridgeEntry, filepath.Join(dir, "bridge-state"), feishubridge.Init{
		Socket: filepath.Join(dir, "bridge.sock"), CallbackSocket: SocketPath(dir), CWD: workspace,
		AppID: appID, AppSecret: appSecret, OwnerOpenID: cfg.HITL.Feishu.OwnerOpenID,
	}, log)
	if err != nil {
		return err
	}
	defer host.client.Close()
	networkPath := options.NetworkStatePath
	if networkPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		networkPath = networkclient.DefaultStatePath(home)
	}
	network := networkclient.NewCollaborationManager(networkPath, cfg, repos, host.gh)
	if err := network.Start(ctx); err != nil {
		return err
	}
	defer network.Stop()
	host.mu.Lock()
	host.network = network
	host.mu.Unlock()
	host.sources, err = bridgePlaneSources(cfg)
	if err != nil {
		return err
	}
	host.ready.Store(true)
	// A bridge resume uses the native session recorded by upstream. No recovered
	// OS PID is inspected or killed, and no synthetic human answer is generated.
	if _, err := coordinator.DB().ExecContext(ctx, `UPDATE loops SET status='queued' WHERE status='running' AND json_extract(metadata_json,'$.bridge') IS NOT NULL`); err != nil {
		return err
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastPlane, lastPR := time.Time{}, time.Time{}
	for ctx.Err() == nil {
		if err := host.dispatchQueued(ctx); err != nil {
			host.warn("bridge task dispatch failed", err)
		}
		if time.Since(lastPlane) >= 10*time.Second && network.Status().CloudReachable {
			if err := host.pollPlane(ctx); err != nil {
				host.warn("Plane dispatch poll failed", err)
			}
			lastPlane = time.Now()
		}
		if time.Since(lastPR) >= time.Minute {
			host.pollPullRequests(ctx)
			lastPR = time.Now()
		}
		select {
		case <-ctx.Done():
		case <-host.client.Done():
			return fmt.Errorf("feishu-codex-bridge exited; inspect %s", filepath.Join(dir, "bridge.log"))
		case err := <-serveErr:
			return err
		case <-tick.C:
		}
	}
	// Keep the callback socket alive while upstream interrupts and closes its
	// native sessions, so the last turn status can still be recorded.
	err = host.client.Close()
	close(stopped)
	return err
}

func (h *bridgeHost) enqueue(ctx context.Context, input worker.EnqueueInput) (storage.LoopRecord, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return storage.LoopRecord{}, fmt.Errorf("task prompt is required")
	}
	if input.ProjectID == "" && len(h.cfg.Projects) > 0 {
		input.ProjectID = h.cfg.Projects[0].ID
	}
	// ProjectID locates the Plane connection and record namespace. It does not
	// select a code repository. Even old clients' Repo is merely task context.
	input.Repo = ""
	metadata, _ := json.Marshal(map[string]any{"worker": input, "bridge": bridgeTask{}})
	metadataJSON := string(metadata)
	id := ""
	if input.StrictDispatchID != "" {
		id = "loop_plane_" + input.StrictDispatchID
	}
	service := loops.Service{DB: h.db, Repos: h.repos}
	return service.Create(ctx, loops.CreateInput{ID: id, ProjectID: input.ProjectID,
		Type: domain.LoopTypeWorker, Target: domain.LoopTarget{TargetType: domain.LoopTargetTypeProject, ProjectID: input.ProjectID},
		Status: domain.LoopStatusQueued, MetadataJSON: &metadataJSON})
}

func readBridgeTask(loop storage.LoopRecord) (worker.EnqueueInput, bridgeTask, bool) {
	var saved struct {
		Worker worker.EnqueueInput `json:"worker"`
		Bridge *bridgeTask         `json:"bridge"`
	}
	if loop.MetadataJSON != nil && json.Unmarshal([]byte(*loop.MetadataJSON), &saved) == nil && saved.Bridge != nil {
		return saved.Worker, *saved.Bridge, true
	}
	return saved.Worker, bridgeTask{}, false
}

func (h *bridgeHost) patch(ctx context.Context, id, status string, state bridgeTask, summary string) error {
	patch, _ := json.Marshal(map[string]any{"bridge": state, "worker": map[string]string{"summary": summary}})
	_, err := h.db.ExecContext(ctx, `UPDATE loops SET status=?, metadata_json=json_patch(COALESCE(metadata_json,'{}'),?), updated_at=? WHERE id=? AND status!='terminated'`,
		status, string(patch), eventlog.FormatJavaScriptISOString(time.Now()), id)
	return err
}

func (h *bridgeHost) dispatchQueued(ctx context.Context) error {
	items, err := h.repos.Loops.ListByStatuses(ctx, []string{"queued"})
	if err != nil {
		return err
	}
	var failures []error
	for _, loop := range items {
		if err := h.dispatchTask(ctx, loop); err != nil {
			failures = append(failures, fmt.Errorf("task %s: %w", loop.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (h *bridgeHost) dispatchTask(ctx context.Context, loop storage.LoopRecord) error {
	input, state, ok := readBridgeTask(loop)
	if !ok {
		return nil // Legacy tasks stay owned by their original executor.
	}
	h.mu.Lock()
	busy := h.active[loop.ID]
	h.mu.Unlock()
	if busy {
		return nil
	}
	if err := h.chats.prepare(ctx, loop.ID); err != nil {
		// Optional invites/announcement edits must not block a task whose
		// group was already created and durably assigned.
		h.warn("Feishu group setup incomplete", err)
	}
	h.notify.RefreshThreadHeader(ctx, loop.ID, nil, 0)
	root, err := h.repos.FeishuThreads.RootByLoop(ctx, loop.ID)
	if err != nil || root == "" {
		return fmt.Errorf("task %s has no Feishu anchor: %v", loop.ID, err)
	}
	chat, err := h.repos.FeishuThreads.ChatIDByLoop(ctx, loop.ID)
	if err != nil {
		return err
	}
	h.mu.Lock()
	fresh, err := h.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" || h.active[loop.ID] {
		h.mu.Unlock()
		if err != nil {
			return err
		}
		return nil
	}
	_, state, _ = readBridgeTask(*fresh)
	state.RootID = root
	err = h.patch(ctx, loop.ID, "running", state, "正在了解任务和影响面")
	if err == nil {
		h.active[loop.ID] = true
	}
	h.mu.Unlock()
	if err != nil {
		return err
	}
	prompt := h.taskPrompt(loop.ID, input)
	if state.SessionID != "" {
		prompt = "继续原任务；先结合会话和实际文件确认已做过的工作，避免重复操作。\n\n" + prompt
	}
	if err := h.client.Request(ctx, http.MethodPost, "/tasks", feishubridge.Task{ChatID: chat,
		ThreadID: state.ThreadID, RootMessageID: root, CWD: filepath.Join(h.dir, "workspace"),
		Title: input.Title, Prompt: prompt, RequesterOpenID: h.cfg.HITL.Feishu.OwnerOpenID,
		Model: value(h.cfg.Agent.Model)}); err != nil {
		h.mu.Lock()
		delete(h.active, loop.ID)
		_ = h.patch(ctx, loop.ID, "queued", state, "等待重新派发")
		h.mu.Unlock()
		return err
	}
	return nil
}

func (h *bridgeHost) turnEvent(w http.ResponseWriter, r *http.Request) {
	var event feishubridge.TurnEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&event); err != nil {
		http.Error(w, "invalid turn event", 400)
		return
	}
	id, err := h.recordTurnEvent(r.Context(), event)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	// Feishu delivery can take seconds. Keep it out of the task-state lock.
	if id != "" && h.notify != nil {
		h.notify.RefreshThreadHeader(r.Context(), id, nil, 0)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (h *bridgeHost) recordTurnEvent(ctx context.Context, event feishubridge.TurnEvent) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	items, err := h.repos.Loops.List(ctx)
	if err != nil {
		return "", err
	}
	for _, loop := range items {
		_, state, ok := readBridgeTask(loop)
		if !ok || loop.Status == "terminated" || !((event.ThreadID != "" && event.ThreadID == state.ThreadID) || (event.RootMessageID != "" && event.RootMessageID == state.RootID)) {
			continue
		}
		if event.ThreadID != "" {
			state.ThreadID = event.ThreadID
		}
		if event.SessionID != "" {
			state.SessionID = event.SessionID
		}
		status, summary := "running", "正在处理"
		if event.Phase == "started" {
			h.active[loop.ID] = true
			if err := os.Remove(h.resultPath(loop.ID)); err != nil && !os.IsNotExist(err) {
				return "", err
			}
		} else if event.Phase == "bound" {
			// Persist the native session as soon as the first card adopts the
			// real Feishu thread, before a host crash can interrupt the turn.
			h.active[loop.ID] = true
		} else {
			delete(h.active, loop.ID)
			status, summary = "failed", "执行中断，可以在原话题 @bot 继续"
			if event.Phase == "finished" && event.Terminal == "done" {
				outcome, readErr := readOutcome(h.resultPath(loop.ID))
				if readErr != nil {
					h.warn("agent task conclusion unavailable", readErr)
					summary = "回复已送达，但任务状态未同步；在原话题 @bot 可继续"
				} else {
					state.Outcome = outcome
					summary = outcome.Summary
					switch outcome.State {
					case "needs_input":
						status = "awaiting_human"
					case "monitoring_prs":
						status = "shepherding"
					case "completed":
						status = "completed"
					}
				}
			}
		}
		if err := h.patch(ctx, loop.ID, status, state, summary); err != nil {
			return "", err
		}
		if status == "awaiting_human" {
			askJSON, err := loops.WriteHITLAsk(loop.MetadataJSON, loops.HITLAsk{Status: "awaiting", MentionOpenIDs: state.Outcome.WaitingFor})
			if err == nil {
				var ask map[string]json.RawMessage
				_ = json.Unmarshal([]byte(askJSON), &ask)
				patch, _ := json.Marshal(map[string]json.RawMessage{"hitl": ask["hitl"]})
				_, _ = h.db.ExecContext(ctx, `UPDATE loops SET metadata_json=json_patch(metadata_json,?) WHERE id=?`, string(patch), loop.ID)
			}
		} else {
			_, _ = h.db.ExecContext(ctx, `UPDATE loops SET metadata_json=json_remove(metadata_json,'$.hitl') WHERE id=?`, loop.ID)
		}
		return loop.ID, nil
	}
	return "", nil
}

func readOutcome(path string) (Outcome, error) {
	file, err := os.Open(path)
	if err != nil {
		return Outcome{}, err
	}
	defer file.Close()
	var outcome Outcome
	if err := json.NewDecoder(io.LimitReader(file, 64<<10)).Decode(&outcome); err != nil {
		return Outcome{}, err
	}
	switch outcome.State {
	case "completed", "needs_input", "monitoring_prs":
		return outcome, nil
	default:
		return Outcome{}, fmt.Errorf("unknown task conclusion %q", outcome.State)
	}
}

func (h *bridgeHost) status(w http.ResponseWriter, r *http.Request) {
	items, err := h.repos.Loops.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	tasks := make([]Task, 0, len(items))
	for _, item := range items {
		tasks = append(tasks, taskStatus(item))
	}
	h.mu.Lock()
	active := len(h.active)
	h.mu.Unlock()
	state := "starting"
	if h.ready.Load() {
		state = "running"
	}
	writeJSON(w, Status{Service: "looper collab · feishu-codex-bridge", PID: os.Getpid(), State: state, ActiveExecutions: active, Tasks: tasks, Network: h.networkStatus()})
}

func (h *bridgeHost) resultPath(id string) string {
	return filepath.Join(h.dir, "workspace", "tasks", id+".json")
}

func (h *bridgeHost) taskPrompt(id string, input worker.EnqueueInput) string {
	return fmt.Sprintf("任务：%s\n来源：%s\n\n%s\n\n本任务状态文件：%s\n每轮结束前，将任务实际结论写到该文件，格式见 AGENTS.md。不要把单轮回复结束当作任务完成。", input.Title, input.IssueURL, input.Prompt, h.resultPath(id))
}

func (h *bridgeHost) writeWorkspace(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0700); err != nil {
		return err
	}
	var repos strings.Builder
	for _, project := range h.cfg.Projects {
		fmt.Fprintf(&repos, "- %s：%s\n", project.Repo, project.RepoPath)
	}
	text := `# Looper 飞书协作

你接手任务后主动推进到实际交付与 PR 收尾。每个话题对应任务和连续会话。

## 仓库

下面是可用仓库，不是任务绑定。先读任务和相关代码，自行判断涉及哪些仓库，允许跨多个仓库。调查后仍有歧义时，在原话题问人。不要求人先选仓库。
` + repos.String() + `
并行修改时按需为各仓创建独立 worktree；不要覆盖别人正在做的修改。当前目录是协作上下文，不是目标代码仓库。

## 工作方式

小修复也先检查原因、调用方和影响面；复杂任务按需拆分、列计划或写技术设计，不强制每项写文档。
在开始、关键发现、方案改变和交付时，用简短中文说明进展。先说结论，使用段落、列表和 Markdown，不贴长工具日志。工具细节由 bridge 折叠。
只有需要人类判断时才等待。找谁由你判断，可查任务、参与者及同事信息；不清楚就问 Looper owner。选择题用编号选项，人在原话题 @bot 回复。
需要邀请同事时，先以 bot 邀请、确认入群，再 @；不要求只允许 owner 回复。飞书读写、附件和表情全部使用 bot，禁止 --as user，权限不足不能切换用户身份。
交付要让验收人实际能访问。按任务选择预览、截图、录屏或包；本机路径和 localhost 不能当作共享链接。优先利用 bridge 的图片与本地文件交付能力。
只读调查、解释和文档任务可以正常完成，不强求代码修改或 PR。开发任务创建所需各仓 PR，持续处理 review、CI、冲突和合并队列。未获授权不要代替人合并。

## 每轮任务结论

任务提示会给出独立的状态文件路径。每轮回复前把实际结论写到该文件；它只同步任务状态，不是审批或新的决策规则。不要在飞书贴 JSON。
格式：{"state":"needs_input","summary":"一句话进展","waitingFor":["负责人 open_id"],"pullRequests":["https://github.com/owner/repo/pull/123"]}
summary 只写一句短进展（约 40 字），解释和证据放话题正文，不要复制整篇回复。
state 可为 needs_input（确实需要人回复）、monitoring_prs（PR 尚在推进或等待合并）、completed（任务已实际完成；有 PR 时应在收尾后）。pullRequests 记录任务涉及的全部仓库 PR，允许为空。
继续原话题时沿用同一个任务文件。普通聊天只有 @bot 才触发你，不要自行轮询飞书抢另一条消息通道。
`
	return os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(text), 0600)
}

func (h *bridgeHost) warn(message string, err error) {
	if h.options.Logger != nil {
		h.options.Logger.Warn(message, map[string]any{"error": err.Error()})
	}
}

// Plane dispatches select a device and task, never a code repository.
func bridgePlaneSources(cfg config.Config) ([]planeSource, error) {
	var result []planeSource
	for _, provider := range cfg.Providers {
		if provider.Kind != config.ProviderKindPlane || provider.StrictDispatch == nil || !provider.StrictDispatch.Enabled {
			continue
		}
		for _, project := range cfg.Projects {
			if project.Provider != provider.ID {
				continue
			}
			client, err := forge.NewPlaneClientFromConfig(provider, project.Repo)
			if err != nil {
				return nil, err
			}
			result = append(result, planeSource{project: project, client: client, nodeID: provider.StrictDispatch.NodeID})
			break // One connection per Plane provider, regardless of repository count.
		}
	}
	return result, nil
}

func (h *bridgeHost) networkStatus() networkclient.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.network == nil {
		return networkclient.Status{}
	}
	return h.network.Status()
}

func (h *bridgeHost) pollPlane(ctx context.Context) error {
	var failures []error
	for _, source := range h.sources {
		inbox, err := source.client.StrictDispatchInbox(ctx)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, dispatch := range inbox.Dispatches {
			if dispatch.RequestedMode != "collab" || dispatch.ActiveRole != "worker" {
				continue
			}
			if err := h.planeDispatch(ctx, source.client, source.project.ID, dispatch); err != nil {
				failures = append(failures, fmt.Errorf("dispatch %s: %w", dispatch.ID, err))
			}
		}
	}
	return errors.Join(failures...)
}

func (h *bridgeHost) planeDispatch(ctx context.Context, client *forge.PlaneClient, project string, dispatch planestrict.Dispatch) error {
	loop, err := h.repos.Loops.GetByID(ctx, "loop_plane_"+dispatch.ID)
	if err != nil {
		return err
	}
	if loop == nil {
		if dispatch.Health != "ok" || (dispatch.State != "queued" && dispatch.State != "claimed") {
			return nil
		}
		issue, err := client.StrictDispatchIssue(ctx, dispatch)
		if err != nil {
			return err
		}
		if dispatch.State == "queued" {
			if err := client.ClaimStrictDispatch(ctx, dispatch); err != nil {
				return err
			}
		}
		input := worker.EnqueueInput{ProjectID: project, Title: issue.Title, Prompt: issue.Body,
			IssueURL: issue.HTMLURL, IssueNumber: issue.Number, StrictDispatchID: dispatch.ID}
		if scope := dispatch.CollaborationScope; scope != nil {
			input.ChatScope, input.ChatTitle, input.ChatURL = scope.Key, scope.Name, client.CollaborationURL(scope)
		}
		_, err = h.enqueue(ctx, input)
		return err
	}
	_, state, ok := readBridgeTask(*loop)
	if !ok {
		return nil
	}
	if dispatch.State == "stop_requested" {
		key := state.ThreadID
		if key == "" {
			key = "pending:" + state.RootID
		}
		if err := h.client.Request(ctx, http.MethodPost, "/stop-task", map[string]string{"threadId": key}); err != nil {
			return err
		}
		h.mu.Lock()
		active := h.active[loop.ID]
		h.mu.Unlock()
		if active {
			return nil // The actual turn-end callback, not the stop request, proves exit.
		}
		if err := h.patch(ctx, loop.ID, "terminated", state, "已从 Plane 停止"); err != nil {
			return err
		}
		return client.TransitionStrictDispatch(ctx, dispatch.ID, "confirmed_stopped", nil)
	}
	status := ""
	var wait *string
	switch loop.Status {
	case "running", "shepherding":
		status = "running"
	case "awaiting_human":
		status = "awaiting_human"
		kind := "role_decision"
		wait = &kind
	case "completed", "failed":
		status = loop.Status
	}
	if status != "" && dispatch.Health == "ok" && dispatch.State != status {
		return client.TransitionStrictDispatch(ctx, dispatch.ID, status, wait)
	}
	return nil
}

func (h *bridgeHost) pollPullRequests(ctx context.Context) {
	items, err := h.repos.Loops.ListByStatuses(ctx, []string{"shepherding"})
	if err != nil {
		return
	}
	for _, loop := range items {
		_, state, ok := readBridgeTask(loop)
		if !ok || len(state.Outcome.PullRequests) == 0 {
			continue
		}
		var signals, notes []string
		allClosed, actionable := true, false
		for _, link := range state.Outcome.PullRequests {
			repo, number, err := parsePullRequestURL(link)
			if err != nil {
				h.warn("invalid agent PR reference", err)
				allClosed = false
				continue
			}
			pr, err := h.gh.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: number})
			if err != nil {
				h.warn("PR status read failed", err)
				allClosed = false
				continue
			}
			signal := workerhost.FoldShepherdSignal(pr)
			signals = append(signals, link+":"+signal)
			if !strings.EqualFold(pr.State, "MERGED") && !strings.EqualFold(pr.State, "CLOSED") {
				allClosed = false
			}
			if workerhost.ShepherdNeedsPass(pr) {
				actionable = true
			}
			// Pass the fetched evidence, not only a wake marker. An agent's
			// managed PR CLI may expose metadata without reviews or CI logs.
			evidence, err := json.Marshal(pr)
			if err != nil {
				h.warn("PR evidence serialization failed", err)
				continue
			}
			notes = append(notes, link+"\n"+string(evidence))
		}
		sort.Strings(signals)
		signal := strings.Join(signals, "\n")
		if signal == state.Signal || (!actionable && !allClosed) {
			continue
		}
		// Wake the same agent even when every PR closes: the agent reconciles
		// the task outcome, rather than treating any closed PR as a business pass.
		prompt := "原任务的 PR 状态有变化。以下是已读取的 PR 现场，包含 Reviews／Comments 原文及 Checks；逐项核对和处理，不因其他 CLI 摘要缺少字段而忽略这些内容。检查冲突及合并队列；若全部 PR 已结束，核实实际交付结论。不要替人合并。用简短中文说明进展，不贴原始快照。\n\n" + strings.Join(notes, "\n\n") + "\n\n本任务状态文件：" + h.resultPath(loop.ID)
		chat, err := h.repos.FeishuThreads.ChatIDByLoop(ctx, loop.ID)
		if err != nil {
			continue
		}
		h.mu.Lock()
		fresh, err := h.repos.Loops.GetByID(ctx, loop.ID)
		if err != nil || fresh == nil || fresh.Status != "shepherding" || h.active[loop.ID] {
			h.mu.Unlock()
			continue
		}
		_, current, _ := readBridgeTask(*fresh)
		if !reflect.DeepEqual(current, state) {
			h.mu.Unlock()
			continue
		}
		state.Signal = signal
		err = h.patch(ctx, loop.ID, "running", state, "正在跟进 PR 变化")
		if err == nil {
			h.active[loop.ID] = true
		}
		h.mu.Unlock()
		if err != nil {
			continue
		}
		if err := h.client.Request(ctx, http.MethodPost, "/tasks", feishubridge.Task{ChatID: chat, ThreadID: state.ThreadID,
			RootMessageID: state.RootID, CWD: filepath.Join(h.dir, "workspace"), Title: "跟进 PR", Prompt: prompt,
			RequesterOpenID: h.cfg.HITL.Feishu.OwnerOpenID}); err != nil {
			h.mu.Lock()
			delete(h.active, loop.ID)
			state.Signal = current.Signal
			_ = h.patch(ctx, loop.ID, "shepherding", state, current.Outcome.Summary)
			h.mu.Unlock()
			h.warn("PR follow-up dispatch failed", err)
			continue
		}
	}
}

func parsePullRequestURL(link string) (string, int64, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return "", 0, fmt.Errorf("not a GitHub PR URL: %s", link)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", 0, fmt.Errorf("not a GitHub PR URL: %s", link)
	}
	number, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || number <= 0 {
		return "", 0, fmt.Errorf("invalid PR number: %s", link)
	}
	return parts[0] + "/" + parts[1], number, nil
}
