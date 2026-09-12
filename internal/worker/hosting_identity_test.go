package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/hostingidentity"
)

type hostingWorkerObservation struct {
	t       *testing.T
	session *hostingidentity.Session
	steps   []string
}

func (o *hostingWorkerObservation) observe(ctx context.Context, step string) {
	o.t.Helper()
	session, ok := hostingidentity.FromContext(ctx)
	if !ok || session.Name() != "worker-bot" || session.Role() != "worker" || session.ProjectID() != "project_1" {
		o.t.Fatalf("%s lost worker selection: %v", step, session)
	}
	if o.session != nil && o.session != session {
		o.t.Fatalf("%s replaced the captured run session", step)
	}
	o.session = session
	o.steps = append(o.steps, step)
}

type hostingWorkerAgent struct {
	*fakeAgentExecutor
	observation *hostingWorkerObservation
}

func (a hostingWorkerAgent) Start(ctx context.Context, input AgentRunInput) (AgentExecution, error) {
	a.observation.observe(ctx, "agent")
	if !strings.Contains(input.Prompt, "$LOOPER_HOST_CLI") || strings.Contains(input.Prompt, "gh pr create") {
		a.observation.t.Fatalf("bot agent did not receive local-work/host-broker prompt: %s", input.Prompt)
	}
	return a.fakeAgentExecutor.Start(ctx, input)
}

type hostingWorkerGit struct {
	*fakeGitGateway
	observation *hostingWorkerObservation
}

func (g hostingWorkerGit) Push(ctx context.Context, input PushInput) error {
	g.observation.observe(ctx, "daemon push")
	return g.fakeGitGateway.Push(ctx, input)
}

type hostingWorkerGitHub struct {
	*fakeGitHubGateway
	observation *hostingWorkerObservation
	reviewerErr error
}

func (g hostingWorkerGitHub) CreatePullRequest(ctx context.Context, input CreatePullRequestInput) (CreatePullRequestResult, error) {
	g.observation.observe(ctx, "daemon create PR")
	return g.fakeGitHubGateway.CreatePullRequest(ctx, input)
}

func (g hostingWorkerGitHub) AddPullRequestReviewers(ctx context.Context, input PullRequestReviewersInput) error {
	g.observation.observe(ctx, "daemon reviewers")
	if g.reviewerErr != nil {
		return g.reviewerErr
	}
	return g.fakeGitHubGateway.AddPullRequestReviewers(ctx, input)
}

func TestHostingIdentityWorkerPublishesWithoutAgentLifecycleAndKeepsSession(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "publishes", true: "metadata_denied"}[denied], func(t *testing.T) {
			fixture := newRunnerFixture(t)
			fixture.cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Repo: "acme/looper", Identity: "worker-bot"}}
			fixture.cfg.Identities = map[string]config.HostingIdentityConfig{"worker-bot": {Kind: config.HostingIdentityGitHubApp, AppID: 1, InstallationID: 2, PrivateKeyFile: "/not-read-by-fake-transports.pem"}}
			observation := &hostingWorkerObservation{t: t}
			git := hostingWorkerGit{fakeGitGateway: &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/feature", BaseBranch: "main", HeadSHA: "abc123", WorktreeID: "worktree_1"}}, observation: observation}
			github := hostingWorkerGitHub{fakeGitHubGateway: &fakeGitHubGateway{createPRResult: CreatePullRequestResult{Number: 101, URL: "https://github.com/acme/looper/pull/101"}}, observation: observation}
			if denied {
				github.reviewerErr = &hostingidentity.Error{Identity: "worker-bot", Operation: "request reviewers", StatusCode: 403, Reason: "permission denied"}
			}
			agent := hostingWorkerAgent{fakeAgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "local implementation completed", ParseStatus: "parsed"}}}, observation: observation}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoCommit: true, AllowAutoPush: true, GitHubCLIAutoPROpeningAvailable: func(context.Context, string, string) bool { return true }, OpenPRStrategy: config.OpenPRStrategyAllDone, CustomInstructions: fixture.cfg})
			claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "worker", "worker")
			if err != nil || claim == nil {
				t.Fatalf("claim: %#v, %v", claim, err)
			}
			payload := strings.TrimSuffix(*claim.PayloadJSON, "}") + `,"reviewers":["review-bot[bot]"]}`
			claim.PayloadJSON = &payload
			result, err := runner.ProcessClaimedItem(context.Background(), *claim)
			if denied {
				if err == nil && (result.Status == "success" || !strings.Contains(result.Summary, `hosting identity "worker-bot"`)) {
					t.Fatalf("bot metadata denial was swallowed: %#v, %v", result, err)
				}
			} else if err != nil || result.Status != "success" || result.PullRequestNumber != 101 {
				t.Fatalf("bot daemon publication without lifecycle: %#v, %v", result, err)
			}
			if len(agent.starts) != 1 || len(git.pushCalls) != 1 || len(github.createPRCalls) != 1 || !strings.Contains(strings.Join(observation.steps, ","), "daemon reviewers") {
				t.Fatalf("agent/publication steps = %v; starts=%d pushes=%d creates=%d", observation.steps, len(agent.starts), len(git.pushCalls), len(github.createPRCalls))
			}
		})
	}
}

func TestHostingIdentityWorkerValidationSanitizesEveryCommand(t *testing.T) {
	for _, name := range []string{"GH_TOKEN", "OTHER_BOT_TOKEN", "SELECTED_BOT_TOKEN", "GIT_CONFIG_COUNT", "SSH_AUTH_SOCK", "LOOPER_TRUSTED_REVIEW_SOCK"} {
		t.Setenv(name, "poison")
	}
	t.Setenv("ANTHROPIC_API_KEY", "model-credential")
	cfg := config.Config{Identities: map[string]config.HostingIdentityConfig{"selected": {TokenEnv: "SELECTED_BOT_TOKEN"}, "other": {TokenEnv: "OTHER_BOT_TOKEN"}}}
	ctx, err := hostingidentity.BindResolved(context.Background(), config.ResolvedHostingIdentity{Name: "worker-bot", Definition: config.HostingIdentityConfig{Kind: config.HostingIdentityGitHubApp, BaseURL: "https://github.com", AppID: 1, InstallationID: 2, PrivateKeyFile: "/missing.pem"}, Target: config.RepositoryIdentity{Kind: config.ProviderKindGitHub, BaseURL: "https://github.com", Repo: "acme/looper"}, ProjectID: "project_1", Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{customInstructions: cfg}
	command := `test -z "$GH_TOKEN$OTHER_BOT_TOKEN$SELECTED_BOT_TOKEN$SSH_AUTH_SOCK$LOOPER_TRUSTED_REVIEW_SOCK" && test "$ANTHROPIC_API_KEY" = model-credential && test -d "$GH_CONFIG_DIR" && test ! -f "$GH_CONFIG_DIR/hosts.yml" && test "$(git config protocol.allow)" = never && ! gh auth status && ! tea login list && ! git fetch origin`
	for i := 0; i < 2; i++ {
		result, err := runner.runValidation(ctx, ValidationInput{CWD: t.TempDir(), Commands: []string{command, command}})
		if err != nil || !result.Passed {
			t.Fatalf("validation attempt %d inherited daemon credentials: %#v, %v", i, result, err)
		}
	}
}

func TestHostingIdentityWorkerPromptUsesAvailablePRContext(t *testing.T) {
	t.Parallel()
	for _, kind := range []config.HostingIdentityKind{config.HostingIdentityGitHubApp, config.HostingIdentityForgejoToken} {
		for _, existingPR := range []bool{false, true} {
			name := string(kind) + "/new_pr"
			if existingPR {
				name = string(kind) + "/existing_pr"
			}
			t.Run(name, func(t *testing.T) {
				work := workerInput{Repo: "acme/looper", Title: "Implement the feature", BaseBranch: "main", Branch: "looper/feature"}
				if existingPR {
					work.ExecutionMode, work.PRNumber, work.HeadSHA = "push-existing", 42, "abc123"
				}
				prompt, _, err := buildWorkerPromptWithInstructions(t.TempDir(), "project", config.Config{}, work, nil, true, config.DefaultDisclosureConfig(), "opencode", "", kind)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{`"$LOOPER_HOST_CLI" host whoami`, "Looper publishes local commits", "__LOOPER_RESULT__"} {
					if !strings.Contains(prompt, want) {
						t.Errorf("worker bot prompt lost %q", want)
					}
				}
				for _, unwanted := range []string{"seeded", "Fail on drift", "Before acting", "pulls/<number>", "pulls/0", "gh pr create"} {
					if strings.Contains(prompt, unwanted) {
						t.Errorf("worker received a missing seed requirement or unavailable command %q", unwanted)
					}
				}
				if existingPR {
					for _, want := range []string{"host api pulls/42.", "host api pulls/42 --diff", "host api pulls/42/reviews --paginate", "Read these when current PR context is needed"} {
						if !strings.Contains(prompt, want) {
							t.Errorf("existing-PR worker lost applicable read %q", want)
						}
					}
				} else {
					for _, unwanted := range []string{"host api pulls/", "host threads ", "host thread "} {
						if strings.Contains(prompt, unwanted) {
							t.Errorf("new-PR worker received unavailable PR read %q", unwanted)
						}
					}
				}
			})
		}
	}
}
