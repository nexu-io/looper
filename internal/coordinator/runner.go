package coordinator

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nexu-io/looper/internal/bootstrap"
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/coordinator/dispatch"
	"github.com/nexu-io/looper/internal/coordinator/triage"
	"github.com/nexu-io/looper/internal/disclosure"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/storage"
)

const jsISOStringLayout = "2006-01-02T15:04:05.000Z"

const triageCommentMarker = "<!-- looper:coordinator:triage -->"
const dispatchFailureCommentMarker = "<!-- looper:coordinator:dispatch-failure -->"

type DiscoveryInput struct {
	ProjectID string
	Repo      string
}

type DiscoveryResult struct {
	Skipped bool
	Ticked  bool
}

type IssueSummary struct {
	Number int64
	Labels []string
}

type GitHubGateway interface {
	ListOpenIssues(context.Context, githubinfra.ListOpenIssuesInput) ([]githubinfra.IssueSummary, error)
	ViewIssue(context.Context, githubinfra.ViewIssueInput) (githubinfra.IssueDetail, error)
	ListIssueComments(context.Context, githubinfra.ViewIssueInput) ([]githubinfra.CommentInfo, error)
	ListIssueTimeline(context.Context, githubinfra.IssueTimelineInput) ([]map[string]any, error)
	GetCurrentUserLogin(context.Context, string) (string, error)
	GetCurrentUserLoginForRepo(context.Context, string, string) (string, error)
	GetRepositoryPermission(context.Context, githubinfra.RepositoryPermissionInput) (string, error)
	AddIssueAssignees(context.Context, githubinfra.IssueAssigneesInput) error
	AddIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
	AddIssueReaction(context.Context, githubinfra.CreateIssueReactionInput) error
	RemoveIssueLabels(context.Context, githubinfra.IssueLabelsInput) error
	CreateIssueComment(context.Context, githubinfra.IssueCommentInput) (githubinfra.IssueCommentResult, error)
	UpdateIssueComment(context.Context, githubinfra.UpdateIssueCommentInput) error
}

type RepositoryInspector interface {
	Inspect(context.Context, string, triage.Issue) (triage.RepoContext, error)
}

type Options struct {
	Repos      *storage.Repositories
	GitHub     GitHubGateway
	Config     *config.Config
	Logger     bootstrap.Logger
	Now        func() time.Time
	TriageLLM  triage.LLM
	Inspector  RepositoryInspector
	Disclosure *config.DisclosureConfig
}

type Runner struct {
	repos      *storage.Repositories
	github     GitHubGateway
	config     *config.Config
	logger     bootstrap.Logger
	now        func() time.Time
	triageLLM  triage.LLM
	inspector  RepositoryInspector
	disclosure *config.DisclosureConfig

	mu                sync.Mutex
	lastTickByProject map[string]time.Time
}

func New(options Options) *Runner {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	inspector := options.Inspector
	if inspector == nil {
		inspector = localRepositoryInspector{}
	}
	return &Runner{
		repos:             options.Repos,
		github:            options.GitHub,
		config:            options.Config,
		logger:            options.Logger,
		now:               now,
		triageLLM:         options.TriageLLM,
		inspector:         inspector,
		disclosure:        options.Disclosure,
		lastTickByProject: map[string]time.Time{},
	}
}

func (r *Runner) DiscoverIssues(ctx context.Context, input DiscoveryInput) (DiscoveryResult, error) {
	if !r.shouldRunTick(input.ProjectID) {
		return DiscoveryResult{Skipped: true}, nil
	}
	if r.github == nil {
		return DiscoveryResult{Ticked: true}, nil
	}
	if r.repos == nil || r.repos.Projects == nil {
		return DiscoveryResult{}, fmt.Errorf("coordinator repositories are not configured")
	}
	project, roleCfg, sweeperCfg, err := r.projectConfig(ctx, input.ProjectID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	if project.Archived || !roleCfg.Enabled {
		return DiscoveryResult{Skipped: true}, nil
	}
	issues, err := r.github.ListOpenIssues(ctx, githubinfra.ListOpenIssuesInput{Repo: input.Repo, CWD: project.RepoPath, Limit: 100})
	if err != nil {
		return DiscoveryResult{}, err
	}
	triageCfg := roleConfigToTriageConfig(roleCfg)
	dispatchCfg := roleConfigToDispatchConfig(roleCfg, config.ProjectRoleConfigs(*r.config, input.ProjectID))
	processed := 0
	for _, summary := range issues {
		if ShouldSkipIssue(IssueSummary{Number: summary.Number, Labels: summary.Labels}, roleCfg, sweeperCfg) {
			continue
		}
		issue, err := r.loadIssue(ctx, input.Repo, project.RepoPath, summary.Number)
		if err != nil {
			return DiscoveryResult{}, err
		}
		dispatchIssue, err := r.dispatchIssue(ctx, input.Repo, project.RepoPath, issue, triageCfg.TriagedLabel, dispatchCfg)
		if err != nil {
			return DiscoveryResult{}, err
		}
		dispatchAction := dispatch.Decide(dispatchIssue, dispatchCfg, r.now().UTC())
		if r.hasDispatchWork(dispatchAction) {
			if err := r.applyDispatchAction(ctx, input.Repo, project.RepoPath, issue, dispatchAction); err != nil {
				return DiscoveryResult{}, err
			}
			if strings.TrimSpace(dispatchAction.FailureCommentBody) == "" {
				continue
			}
		}
		if processed >= triageCfg.MaxPerTick {
			continue
		}
		if !mightNeedCoordinatorAction(summary, triageCfg) {
			continue
		}
		if !triage.ShouldReTriage(issue, triageCfg, r.now().UTC()) && !triage.ShouldTriage(issue, triageCfg, r.now().UTC()) {
			continue
		}
		analysisStartedAt := r.now().UTC()
		processed++
		decision, err := r.decide(ctx, project.RepoPath, input.Repo, issue, triageCfg)
		if err != nil {
			return DiscoveryResult{}, err
		}
		if decision.NoOp {
			continue
		}
		if err := r.applyDecision(ctx, input.Repo, project.RepoPath, issue, triageCfg, analysisStartedAt, decision); err != nil {
			return DiscoveryResult{}, err
		}
	}
	return DiscoveryResult{Ticked: true}, nil
}

func (r *Runner) hasDispatchWork(action dispatch.Action) bool {
	return action.ReactionCommentID != 0 || len(action.TriggerLabels) != 0 || action.FailureCommentBody != ""
}

// ShouldSkipIssue reserves the structural cross-role boundary with Sweeper.
// Future triage discovery must skip issues that Sweeper already marked pending,
// retired, or quarantined so the two roles never fight over authority.
func ShouldSkipIssue(issue IssueSummary, roleCfg config.CoordinatorRoleConfig, sweeperCfg config.SweeperRoleConfig) bool {
	_ = roleCfg
	return hasExactLabel(issue.Labels, sweeperCfg.Lifecycle.PendingLabel) ||
		hasExactLabel(issue.Labels, sweeperCfg.Lifecycle.ClosedLabel) ||
		hasExactLabel(issue.Labels, sweeperCfg.Security.QuarantineLabel)
}

func (r *Runner) decide(ctx context.Context, repoPath string, repo string, issue triage.Issue, cfg triage.Config) (triage.Decision, error) {
	if triage.ShouldReTriage(issue, cfg, r.now().UTC()) {
		return triage.ReTriageDecision(cfg), nil
	}
	if !triage.ShouldTriage(issue, cfg, r.now().UTC()) {
		return triage.NoOpDecision(), nil
	}
	repoCtx, err := r.inspector.Inspect(ctx, repoPath, issue)
	if err != nil {
		return triage.Decision{}, err
	}
	repoCtx.Repo = repo
	repoCtx.WorkingDirectory = repoPath
	return triage.Decide(ctx, r.triageLLM, triage.Input{Issue: issue, RepoContext: repoCtx, Config: cfg, Now: r.now().UTC()}), nil
}

func (r *Runner) applyDecision(ctx context.Context, repo string, cwd string, issue triage.Issue, cfg triage.Config, analysisStartedAt time.Time, decision triage.Decision) error {
	if err := r.removeIssueLabels(ctx, repo, cwd, issue.Number, issue.Labels, decision.ClearLabelPatterns); err != nil {
		return err
	}
	if err := r.removeIssueLabels(ctx, repo, cwd, issue.Number, issue.Labels, decision.RemoveLabels); err != nil {
		return err
	}
	applyNow := removeExactLabels(decision.ApplyLabels, cfg.TriagedLabel)
	if len(applyNow) > 0 {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: applyNow, CWD: cwd}); err != nil {
			return err
		}
	}
	if strings.TrimSpace(decision.CommentBody) != "" {
		if err := r.postOrEditComment(ctx, repo, cwd, issue, analysisStartedAt, decision.CommentBody); err != nil {
			return err
		}
	}
	if decision.MarkTriaged && !hasExactLabel(issue.Labels, cfg.TriagedLabel) {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: []string{cfg.TriagedLabel}, CWD: cwd}); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) applyDispatchAction(ctx context.Context, repo string, cwd string, issue triage.Issue, action dispatch.Action) error {
	if strings.TrimSpace(action.FailureCommentBody) != "" {
		if err := r.postOrEditDispatchFailureComment(ctx, repo, cwd, issue.Number, action.FailureCommentBody); err != nil {
			return err
		}
		if action.ReactionCommentID != 0 && strings.TrimSpace(action.ReactionContent) != "" {
			return r.github.AddIssueReaction(ctx, githubinfra.CreateIssueReactionInput{Repo: repo, CommentID: action.ReactionCommentID, Content: action.ReactionContent, CWD: cwd})
		}
		return nil
	}
	if strings.TrimSpace(action.AssignTo) != "" {
		if err := r.github.AddIssueAssignees(ctx, githubinfra.IssueAssigneesInput{Repo: repo, IssueNumber: issue.Number, Assignees: []string{action.AssignTo}, CWD: cwd}); err != nil {
			return err
		}
	}
	labelsToAdd := removeExistingLabels(action.TriggerLabels, issue.Labels)
	if len(labelsToAdd) > 0 {
		if err := r.github.AddIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issue.Number, Labels: labelsToAdd, CWD: cwd}); err != nil {
			return err
		}
	}
	if action.ReactionCommentID != 0 && strings.TrimSpace(action.ReactionContent) != "" {
		if err := r.github.AddIssueReaction(ctx, githubinfra.CreateIssueReactionInput{Repo: repo, CommentID: action.ReactionCommentID, Content: action.ReactionContent, CWD: cwd}); err != nil {
			return err
		}
	}
	return nil

}

func (r *Runner) postOrEditComment(ctx context.Context, repo, cwd string, issue triage.Issue, analysisStartedAt time.Time, body string) error {
	comments, err := r.github.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issue.Number, CWD: cwd})
	if err != nil {
		return err
	}
	currentLogin, err := r.github.GetCurrentUserLoginForRepo(ctx, repo, cwd)
	if err != nil {
		return err
	}
	existing := findMarkerComment(comments, currentLogin)
	if hasNewHumanComment(comments, analysisStartedAt) {
		return nil
	}
	commentBody := triageCommentMarker + "\n\n" + body
	stamper := disclosure.FromConfig(*r.config)
	commentBody = stamper.Markdown(commentBody, "coordinator", disclosure.ChannelIssueComment)
	if existing != nil {
		return r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: repo, CommentID: existing.ID, Body: commentBody, CWD: cwd})
	}
	_, err = r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: repo, IssueNumber: issue.Number, Body: commentBody, CWD: cwd})
	return err
}

func (r *Runner) postOrEditDispatchFailureComment(ctx context.Context, repo, cwd string, issueNumber int64, body string) error {
	comments, err := r.github.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
	if err != nil {
		return err
	}
	currentLogin, err := r.github.GetCurrentUserLoginForRepo(ctx, repo, cwd)
	if err != nil {
		return err
	}
	existing := findDispatchFailureComment(comments, currentLogin)
	commentBody := dispatchFailureCommentMarker + "\n\n" + strings.TrimSpace(body)
	stamper := disclosure.FromConfig(*r.config)
	commentBody = stamper.Markdown(commentBody, "coordinator", disclosure.ChannelIssueComment)
	if existing != nil {
		return r.github.UpdateIssueComment(ctx, githubinfra.UpdateIssueCommentInput{Repo: repo, CommentID: existing.ID, Body: commentBody, CWD: cwd})
	}
	_, err = r.github.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: repo, IssueNumber: issueNumber, Body: commentBody, CWD: cwd})
	return err
}

func (r *Runner) removeIssueLabels(ctx context.Context, repo, cwd string, issueNumber int64, existing []string, patterns []string) error {
	labels := matchingLabels(existing, patterns)
	if len(labels) == 0 {
		return nil
	}
	return r.github.RemoveIssueLabels(ctx, githubinfra.IssueLabelsInput{Repo: repo, IssueNumber: issueNumber, Labels: labels, CWD: cwd})
}

func (r *Runner) loadIssue(ctx context.Context, repo, cwd string, issueNumber int64) (triage.Issue, error) {
	detail, err := r.github.ViewIssue(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
	if err != nil {
		return triage.Issue{}, err
	}
	timeline, err := r.github.ListIssueTimeline(ctx, githubinfra.IssueTimelineInput{Repo: repo, IssueNumber: issueNumber, CWD: cwd})
	if err != nil {
		return triage.Issue{}, err
	}
	issue := triage.Issue{
		Number:    detail.Number,
		Title:     detail.Title,
		Body:      detail.Body,
		URL:       detail.URL,
		Author:    detail.Author,
		CreatedAt: detail.CreatedAt,
		UpdatedAt: detail.UpdatedAt,
		Labels:    append([]string(nil), detail.Labels...),
		Comments:  make([]triage.Comment, 0, len(detail.Comments)),
		Timeline:  make([]triage.TimelineEvent, 0, len(timeline)),
	}
	for _, comment := range detail.Comments {
		issue.Comments = append(issue.Comments, triage.Comment{ID: comment.ID, Author: comment.Author, AuthorAssociation: comment.AuthorAssociation, Body: comment.Body, CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt})
	}
	for _, event := range timeline {
		issue.Timeline = append(issue.Timeline, triage.TimelineEvent{Event: strings.TrimSpace(asString(event["event"])), CreatedAt: firstNonEmpty(asString(event["created_at"]), asString(event["createdAt"])), Label: timelineLabelName(event)})
	}
	return issue, nil
}

func roleConfigToDispatchConfig(roleCfg config.CoordinatorRoleConfig, roles config.RoleConfigs) dispatch.Config {
	return dispatch.Config{
		Mode:                 roleCfg.Dispatch.Mode,
		TriagedLabel:         roleCfg.Triage.TriagedLabel,
		HoldLabel:            roleCfg.Dispatch.Autonomous.HoldLabel,
		AutonomousDelay:      time.Duration(roleCfg.Dispatch.Autonomous.DelayMinutes) * time.Minute,
		AllowedUsers:         append([]string(nil), roleCfg.Dispatch.HumanGate.AllowedUsers...),
		SlashCommands:        append([]string(nil), roleCfg.Dispatch.HumanGate.SlashCommands...),
		AssignTo:             roleCfg.Dispatch.AssignTo,
		PlannerTriggerLabels: requiredTriggerLabels(roles.Planner.Triggers),
		WorkerTriggerLabels:  requiredTriggerLabels(roles.Worker.Triggers),
	}
}

func requiredTriggerLabels(cfg config.IssueRoleTriggersConfig) []string {
	if cfg.LabelMode == config.LabelModeAll {
		return append([]string(nil), cfg.Labels...)
	}
	if len(cfg.Labels) == 0 {
		return nil
	}
	return []string{cfg.Labels[0]}
}

func (r *Runner) dispatchIssue(ctx context.Context, repo, cwd string, issue triage.Issue, triagedLabel string, cfg dispatch.Config) (dispatch.Issue, error) {
	out := dispatch.Issue{Number: issue.Number, Labels: append([]string(nil), issue.Labels...), Comments: make([]dispatch.Comment, 0, len(issue.Comments))}
	permissionCache := map[string]bool{}
	for _, comment := range issue.Comments {
		createdAt, _ := parseCoordinatorTime(comment.CreatedAt)
		hasWriteAccess := false
		if cfg.Mode == dispatch.ModeHumanGated {
			if _, ok := dispatch.ParseSlashCommand(comment.Body, cfg.SlashCommands); ok {
				allowed, err := r.commentHasWriteAccess(ctx, repo, cwd, comment.Author, permissionCache, cfg)
				if err != nil {
					return dispatch.Issue{}, err
				}
				hasWriteAccess = allowed
			}
		}
		out.Comments = append(out.Comments, dispatch.Comment{ID: comment.ID, Author: comment.Author, AuthorAssociation: comment.AuthorAssociation, HasWriteAccess: hasWriteAccess, Body: comment.Body, CreatedAt: createdAt})
	}
	for _, event := range issue.Timeline {
		if event.Event != "labeled" || event.Label != triagedLabel {
			continue
		}
		when, ok := parseCoordinatorTime(event.CreatedAt)
		if ok && (out.TriagedAt.IsZero() || when.After(out.TriagedAt)) {
			out.TriagedAt = when
		}
	}
	return out, nil
}

func (r *Runner) commentHasWriteAccess(ctx context.Context, repo, cwd, author string, cache map[string]bool, cfg dispatch.Config) (bool, error) {
	author = strings.TrimSpace(author)
	if author == "" {
		return false, nil
	}
	for _, allowed := range cfg.AllowedUsers {
		if strings.EqualFold(strings.TrimSpace(allowed), author) {
			return true, nil
		}
	}
	if allowed, ok := cache[strings.ToLower(author)]; ok {
		return allowed, nil
	}
	permission, err := r.github.GetRepositoryPermission(ctx, githubinfra.RepositoryPermissionInput{Repo: repo, User: author, CWD: cwd})
	if err != nil {
		return false, err
	}
	allowed := permission == "admin" || permission == "maintain" || permission == "write"
	cache[strings.ToLower(author)] = allowed
	return allowed, nil
}

func (r *Runner) projectConfig(ctx context.Context, projectID string) (*storage.ProjectRecord, config.CoordinatorRoleConfig, config.SweeperRoleConfig, error) {
	project, err := r.repos.Projects.GetByID(ctx, projectID)
	if err != nil {
		return nil, config.CoordinatorRoleConfig{}, config.SweeperRoleConfig{}, err
	}
	if project == nil {
		return nil, config.CoordinatorRoleConfig{}, config.SweeperRoleConfig{}, fmt.Errorf("project %q not found", projectID)
	}
	roles := config.ProjectRoleConfigs(*r.config, projectID)
	return project, roles.Coordinator, roles.Sweeper, nil
}

func (r *Runner) shouldRunTick(projectID string) bool {
	interval := r.pollInterval(projectID)
	if interval <= 0 {
		return true
	}
	now := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	lastRun, ok := r.lastTickByProject[projectID]
	if ok && now.Sub(lastRun) < interval {
		return false
	}
	r.lastTickByProject[projectID] = now
	return true
}

func (r *Runner) pollInterval(projectID string) time.Duration {
	if r == nil || r.config == nil {
		return 0
	}
	roleCfg := config.ProjectRoleConfigs(*r.config, projectID).Coordinator
	interval, err := time.ParseDuration(strings.TrimSpace(roleCfg.PollInterval))
	if err != nil {
		return 0
	}
	return interval
}

func roleConfigToTriageConfig(roleCfg config.CoordinatorRoleConfig) triage.Config {
	return triage.Config{
		TriagedLabel:          roleCfg.Triage.TriagedLabel,
		MaxIssueAgeDays:       roleCfg.Triage.MaxIssueAgeDays,
		MaxPerTick:            roleCfg.Triage.MaxPerTick,
		OutOfScopeLabel:       roleCfg.Triage.Disposition.OutOfScopeLabel,
		UnclearLabel:          roleCfg.Triage.Disposition.UnclearLabel,
		ReTriageOnAuthorReply: roleCfg.Triage.Disposition.ReTriageOnAuthorReply,
	}
}

func mightNeedCoordinatorAction(issue githubinfra.IssueSummary, cfg triage.Config) bool {
	return !hasExactLabel(issue.Labels, cfg.TriagedLabel) || hasExactLabel(issue.Labels, cfg.UnclearLabel)
}

func matchingLabels(existing []string, patterns []string) []string {
	matched := []string{}
	for _, label := range existing {
		for _, pattern := range patterns {
			if labelMatchesPattern(label, pattern) {
				matched = append(matched, label)
				break
			}
		}
	}
	return matched
}

func removeExactLabels(labels []string, target string) []string {
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if label != target {
			result = append(result, label)
		}
	}
	return result
}

func removeExistingLabels(labels []string, existing []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if !hasExactLabel(existing, label) {
			out = append(out, label)
		}
	}
	return out
}

func labelMatchesPattern(label string, pattern string) bool {
	if strings.HasSuffix(pattern, "/*") {
		return strings.HasPrefix(label, strings.TrimSuffix(pattern, "*"))
	}
	return label == pattern
}

func hasExactLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func findMarkerComment(comments []githubinfra.CommentInfo, currentLogin string) *githubinfra.CommentInfo {
	for index := range comments {
		if !strings.Contains(comments[index].Body, triageCommentMarker) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(comments[index].Author), strings.TrimSpace(currentLogin)) {
			return &comments[index]
		}
	}
	return nil
}

func findDispatchFailureComment(comments []githubinfra.CommentInfo, currentLogin string) *githubinfra.CommentInfo {
	for index := range comments {
		if !strings.Contains(comments[index].Body, dispatchFailureCommentMarker) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(comments[index].Author), strings.TrimSpace(currentLogin)) {
			return &comments[index]
		}
	}
	return nil
}

func hasNewHumanComment(comments []githubinfra.CommentInfo, since time.Time) bool {
	for _, comment := range comments {
		if strings.Contains(comment.Body, triageCommentMarker) || disclosure.HasMarkdownStamp(comment.Body) {
			continue
		}
		when, ok := parseCoordinatorTime(comment.CreatedAt)
		if ok && when.After(since) {
			return true
		}
	}
	return false
}

func timelineLabelName(event map[string]any) string {
	label, _ := event["label"].(map[string]any)
	return strings.TrimSpace(asString(label["name"]))
}

func parseCoordinatorTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, jsISOStringLayout} {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func asString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type localRepositoryInspector struct{}

func (localRepositoryInspector) Inspect(_ context.Context, repoPath string, issue triage.Issue) (triage.RepoContext, error) {
	ctx := triage.RepoContext{WorkingDirectory: repoPath}
	tokens := triage.SearchTokens(issue)
	if repoPath == "" {
		return ctx, nil
	}
	_ = filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || len(ctx.Paths) >= 12 && len(ctx.Symbols) >= 12 {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".go" && ext != ".md" && ext != ".txt" && ext != ".json" && ext != ".yaml" && ext != ".yml" && ext != ".toml" {
			return nil
		}
		rel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			rel = path
		}
		lowerRel := strings.ToLower(rel)
		for _, token := range tokens {
			if strings.Contains(lowerRel, token) {
				if len(ctx.Paths) < 12 {
					ctx.Paths = append(ctx.Paths, rel)
				}
				break
			}
		}
		if len(ctx.Symbols) >= 12 {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr == nil && info.Size() > 256*1024 {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "func ") && !strings.HasPrefix(trimmed, "type ") && !strings.HasPrefix(trimmed, "const ") && !strings.HasPrefix(trimmed, "var ") {
				continue
			}
			lowerLine := strings.ToLower(trimmed)
			for _, token := range tokens {
				if strings.Contains(lowerLine, token) {
					ctx.Symbols = append(ctx.Symbols, rel+": "+trimmed)
					return nil
				}
			}
		}
		return nil
	})
	return ctx, nil
}
