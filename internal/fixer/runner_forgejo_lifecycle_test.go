package fixer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

type nativeLifecycleGateway struct {
	fakeGitHubGateway
	detail             PullRequestDetail
	probe              forge.ProbeState
	acceptFailedCreate bool
}

func (g *nativeLifecycleGateway) ViewPullRequest(context.Context, ViewPullRequestInput) (PullRequestDetail, error) {
	return g.detail, nil
}
func (g *nativeLifecycleGateway) ProbeNativeReviewCommentResolution(context.Context, ListNativeReviewCommentsInput) (forge.ProbeState, error) {
	return g.probe, nil
}
func (g *nativeLifecycleGateway) CreateIssueComment(ctx context.Context, input IssueCommentInput) (IssueCommentResult, error) {
	created, err := g.fakeGitHubGateway.CreateIssueComment(ctx, input)
	if err == nil || g.acceptFailedCreate {
		if created.ID == 0 {
			created = IssueCommentResult{ID: 9000, URL: "https://example.test/c/9000"}
		}
		g.detail.IssueComments = append(g.detail.IssueComments, map[string]any{"id": created.ID, "url": created.URL, "body": input.Body, "author": map[string]any{"login": "looper"}})
	}
	return created, err
}
func (g *nativeLifecycleGateway) UpdateIssueComment(ctx context.Context, input UpdateIssueCommentInput) error {
	if err := g.fakeGitHubGateway.UpdateIssueComment(ctx, input); err != nil {
		return err
	}
	for _, comment := range g.detail.IssueComments {
		if issueCommentDatabaseID(comment) == input.CommentID {
			comment["body"] = input.Body
		}
	}
	return nil
}

type nativeLifecycleGit struct {
	fakeGitGateway
	remote    *nativeLifecycleGateway
	afterPush func()
}

func (g *nativeLifecycleGit) Push(ctx context.Context, input PushInput) error {
	if err := g.fakeGitGateway.Push(ctx, input); err != nil {
		return err
	}
	g.remote.detail.HeadSHA = "fixed-head"
	if g.afterPush != nil {
		g.afterPush()
	}
	return nil
}

func ownNativeFinding(id int64, updated string) NativeReviewComment {
	return NativeReviewComment{ProviderCommentID: id, Body: "Fix the calculation.\n<!-- looper:stamp -->", Author: "looper", ResolverPresent: true, Path: "price.go", URL: fmt.Sprintf("https://forge.example/acme/looper/pulls/42#issuecomment-%d", id), UpdatedAt: updated, ObservedFingerprint: NativeReviewCommentFingerprint(id, updated), ReviewAuthor: "looper", ReviewState: "COMMENTED", ReviewCommitID: "head-1", ReviewBody: "Please fix the calculation.\n<!-- looper:review id=reviewer:review-loop head=head-1 outcome=blocking -->"}
}

func nativeAgentResult(items []NativeReviewComment, action string) AgentResult {
	results := make([]map[string]any, 0, len(items))
	for _, item := range items {
		results = append(results, map[string]any{"source": NativeReviewCommentSource, "providerCommentId": item.ProviderCommentID, "action": action, "explanation": "Corrected the discount calculation and checked the boundary cases.", "observedFingerprint": item.ObservedFingerprint})
	}
	payload, _ := json.Marshal(map[string]any{"summary": "Evaluated the calculation feedback.", "repair_results": results})
	return AgentResult{Status: "completed", Summary: "Evaluated the calculation feedback.", ParseStatus: "parsed", Stdout: "__LOOPER_RESULT__=" + string(payload)}
}

func nativeLifecycleRunner(t *testing.T, fixture *runnerFixture, gateway *nativeLifecycleGateway, git *nativeLifecycleGit, agent *fakeAgentExecutor) *Runner {
	t.Helper()
	cfg := forgejoFixerDiscoveryConfig(t, fixture)
	return New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: gateway, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, AllowAutoCommit: true, AllowAutoPush: true, AllowRiskyFixes: true, Logger: fixture.logger, Now: fixture.now, CustomInstructions: cfg})
}

func TestForgejoNativeLifecycleWithoutResolveSurvivesRestartAndLaterFix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRunnerFixture(t)
	finding := ownNativeFinding(101, "u1")
	github := &nativeLifecycleGateway{probe: forge.ProbeStateUnsupported, detail: PullRequestDetail{Number: 42, URL: "https://forge.example/acme/looper/pulls/42", State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix", BaseRefName: "main", BaseSHA: "base-1", Author: "looper"}, fakeGitHubGateway: fakeGitHubGateway{currentUser: "looper", listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1", Author: "looper"}}, nativeComments: []NativeReviewComment{finding}}}
	git := &nativeLifecycleGit{remote: github, fakeGitGateway: fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/fix", HeadSHA: "head-1"}, prepareResult: PrepareWorktreeResult{HeadSHA: "head-1", Clean: true}, inspectResults: []InspectHeadResult{{HeadSHA: "head-1"}, {HeadSHA: "fixed-head", NewCommitSHAs: []string{"fixed-head"}}, {HeadSHA: "fixed-head"}}}}
	agent := &fakeAgentExecutor{results: []AgentResult{nativeAgentResult([]NativeReviewComment{finding}, "fixed")}}
	runner := nativeLifecycleRunner(t, fixture, github, git, agent)
	discovered, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil || len(discovered.QueueItems) != 1 {
		t.Fatalf("discover = %#v, %v", discovered, err)
	}
	fixture.advance(time.Hour)
	claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "test-fixer", "fixer")
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	result, err := runner.ProcessClaimedItem(ctx, *claim)
	if err != nil || result.Status != "success" {
		t.Fatalf("process = %#v, %v", result, err)
	}
	if len(agent.starts) != 1 || len(git.pushCalls) != 1 || len(github.resolveNativeCalls) != 0 {
		t.Fatalf("agent/push/resolve = %d/%d/%d", len(agent.starts), len(git.pushCalls), len(github.resolveNativeCalls))
	}
	run, _ := fixture.repos.Runs.GetByID(ctx, result.RunID)
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.Detail.URL != github.detail.URL || checkpoint.ResolvedComments.Items[0].Status != forgejoFixedUnresolvedStatus || len(checkpoint.Recheck.RemainingFixItems) != 0 {
		t.Fatalf("checkpoint = %#v", checkpoint)
	}
	if len(github.createIssueComments) != 1 || !strings.Contains(github.createIssueComments[0].Body, "code fixed; comment remains open") || strings.Contains(github.createIssueComments[0].Body, "Forgejo Fixer Summary") {
		t.Fatalf("comments = %#v", github.createIssueComments)
	}
	loop, _ := fixture.repos.Loops.GetByID(ctx, result.LoopID)
	item := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{finding}), nil, false)[0]
	entry, ok := findThreadFixEvidence(loadFixEvidenceStoreV2(loop.MetadataJSON), item)
	if !ok || entry.ResolveState != forgejoFixedUnresolvedStatus || entry.Source != "agent_repair_result" {
		t.Fatalf("acknowledgement = %#v, %v", entry, ok)
	}

	// Reopen the real SQLite database: no in-memory cache or preceding run
	// checkpoint should be needed to avoid another repair after daemon restart.
	var sequence int
	var name, dbPath string
	if err := fixture.coordinator.DB().QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	if err := fixture.coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	coordinator, err := storage.OpenSQLiteCoordinator(ctx, dbPath, storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	fixture.coordinator, fixture.repos = coordinator, storage.NewRepositories(coordinator.DB())
	runner = nativeLifecycleRunner(t, fixture, github, git, agent)
	for poll := 0; poll < 3; poll++ {
		discovered, err = runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
		if err != nil || len(discovered.QueueItems) != 0 {
			t.Fatalf("restart poll = %#v, %v", discovered, err)
		}
	}
	// A later unrelated push record must neither forget the old decision nor
	// claim that a newly discovered finding was fixed.
	other := ownNativeFinding(102, "u2")
	otherItem := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{other}), nil, false)[0]
	if err := runner.persistFixEvidenceStoreV2(ctx, *loop, upsertThreadFixEvidence(nil, threadFixEvidence{ThreadID: otherItem.ThreadID, ThreadFingerprint: otherItem.ObservedFingerprint, EvidenceHeadSHA: "later-head", Source: "fallback_push", ResolveState: "pending"})); err != nil {
		t.Fatal(err)
	}
	github.detail.HeadSHA, github.compareStatus = "later-head", "ahead"
	github.nativeComments = append(github.nativeComments, other)
	discovered, err = runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil || len(discovered.QueueItems) != 1 {
		t.Fatalf("new feedback discovery = %#v, %v", discovered, err)
	}
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
	collected, err := runner.runCollectFixesStep(ctx, stepInput{Project: *project, Loop: *loop, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: pullRequestCheckpointDetail(github.detail)}})
	if err != nil || len(collected.FixItems) != 1 || collected.FixItems[0].ProviderCommentID != 102 {
		t.Fatalf("later collect = %#v, %v", collected.FixItems, err)
	}
}

func TestForgejoNativeAcknowledgementOnlySuppressesExactAgentDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	finding := ownNativeFinding(101, "u1")
	item := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{finding}), nil, false)[0]
	for _, tc := range []struct {
		name, state, source, head, compare, fingerprint string
		want                                            int
	}{
		{"same head", forgejoFixedUnresolvedStatus, "agent_repair_result", "fixed-head", "", item.ObservedFingerprint, 0},
		{"descendant", forgejoFixedUnresolvedStatus, "agent_repair_result", "later-head", "ahead", item.ObservedFingerprint, 0},
		{"force push", forgejoFixedUnresolvedStatus, "agent_repair_result", "rewritten-head", "diverged", item.ObservedFingerprint, 1},
		{"edited", forgejoFixedUnresolvedStatus, "agent_repair_result", "fixed-head", "", NativeReviewCommentFingerprint(101, "u2"), 1},
		{"pending push", "pending", "fallback_push", "fixed-head", "", item.ObservedFingerprint, 1},
		{"push not agent", forgejoFixedUnresolvedStatus, "fallback_push", "fixed-head", "", item.ObservedFingerprint, 1},
		{"declined", "declined", "agent_repair_result", "fixed-head", "", item.ObservedFingerprint, 1},
		{"deferred", "deferred", "agent_repair_result", "fixed-head", "", item.ObservedFingerprint, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := upsertThreadFixEvidence(nil, threadFixEvidence{ThreadID: item.ThreadID, ThreadFingerprint: tc.fingerprint, EvidenceHeadSHA: "fixed-head", Source: tc.source, ResolveState: tc.state, Explanation: "Fixed the calculation."})
			metadata := mustJSON(t, map[string]any{"fixEvidenceStoreV2": store})
			gateway := &fakeGitHubGateway{compareStatus: tc.compare}
			runner := New(Options{GitHub: gateway})
			got, err := runner.suppressFixedForgejoNativeItems(ctx, storage.ProjectRecord{RepoPath: t.TempDir()}, "acme/looper", 42, tc.head, &storage.LoopRecord{MetadataJSON: &metadata}, []FixItem{item})
			if err != nil || len(got) != tc.want {
				t.Fatalf("remaining = %#v, %v, want %d", got, err, tc.want)
			}
		})
	}
}

func TestForgejoNativeSummaryFailureReplaysAfterRestartWithoutRepairingAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                          string
		accepted, update, interrupted bool
		wantCreates                   int
	}{
		{name: "create 503", wantCreates: 2},
		{name: "create accepted response lost", accepted: true, wantCreates: 1},
		{name: "update 503", update: true},
		{name: "interrupted after accepted create", accepted: true, interrupted: true, wantCreates: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRunnerFixture(t)
			finding := ownNativeFinding(101, "u1")
			gateway := &nativeLifecycleGateway{probe: forge.ProbeStateUnsupported, acceptFailedCreate: tc.accepted, detail: PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "head-1", HeadRefName: "feature/fix", BaseRefName: "main", BaseSHA: "base-1", Author: "looper"}, fakeGitHubGateway: fakeGitHubGateway{currentUser: "looper", listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1", Author: "looper"}}, nativeComments: []NativeReviewComment{finding}}}
			outage := &forge.ForgejoHTTPError{StatusCode: 503, Method: "POST", Path: "/comments", Message: "temporary outage"}
			if tc.update {
				gateway.updateIssueCommentErr = outage
				gateway.detail.IssueComments = []map[string]any{{"id": int64(9000), "author": map[string]any{"login": "looper"}, "body": fixerRoundSummaryMarker("fixed-head") + "\nPrior acknowledgement"}}
			} else {
				gateway.createIssueCommentErr = outage
			}
			git := &nativeLifecycleGit{remote: gateway, fakeGitGateway: fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/fix", HeadSHA: "head-1"}, prepareResult: PrepareWorktreeResult{HeadSHA: "head-1", Clean: true}, inspectResults: []InspectHeadResult{{HeadSHA: "head-1"}, {HeadSHA: "fixed-head", NewCommitSHAs: []string{"fixed-head"}}, {HeadSHA: "fixed-head"}}}}
			agent := &fakeAgentExecutor{results: []AgentResult{nativeAgentResult([]NativeReviewComment{finding}, "fixed")}}
			validations := 0
			newRunner := func() *Runner {
				runner := nativeLifecycleRunner(t, fixture, gateway, git, agent)
				runner.validationRunner = func(ctx context.Context, input ValidationInput) (ValidationResult, error) {
					validations++
					return passValidation(ctx, input)
				}
				return runner
			}
			runner := newRunner()
			if _, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
				t.Fatal(err)
			}
			fixture.advance(time.Hour)
			claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "test-fixer", "fixer")
			if err != nil || claim == nil {
				t.Fatalf("claim = %#v, %v", claim, err)
			}
			failed, err := runner.ProcessClaimedItem(ctx, *claim)
			if err != nil || failed.Status != "failed" || failed.FailureKind != FailureRetryableAfterResume {
				t.Fatalf("publication failure = %#v, %v", failed, err)
			}
			run, _ := fixture.repos.Runs.GetByID(ctx, failed.RunID)
			checkpoint := parseCheckpoint(run.CheckpointJSON)
			if checkpoint.ResumePolicy != loops.ResumePolicyReplayStep || checkpoint.ResolvedComments.Items[0].Status != forgejoFixedUnresolvedStatus || checkpoint.Repair.ReplyExplanations[0].ObservedFingerprint != finding.ObservedFingerprint {
				t.Fatalf("pending publication lost structured decision: %#v", checkpoint)
			}
			loop, _ := fixture.repos.Loops.GetByID(ctx, failed.LoopID)
			item := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{finding}), nil, false)[0]
			if entry, ok := findThreadFixEvidence(loadFixEvidenceStoreV2(loop.MetadataJSON), item); ok && entry.ResolveState == forgejoFixedUnresolvedStatus {
				t.Fatal("failed publication prematurely suppressed discovery")
			}
			if tc.interrupted {
				// The server accepted the request, but the process exited before
				// persisting its response. The per-item checkpoint already exists.
				run.Status = "interrupted"
				checkpoint.SummaryComment = nil
				encoded := mustMarshalJSON(checkpoint)
				run.CheckpointJSON = &encoded
				if err := fixture.repos.Runs.Upsert(ctx, *run); err != nil {
					t.Fatal(err)
				}
			}
			var sequence int
			var name, dbPath string
			if err := fixture.coordinator.DB().QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &dbPath); err != nil {
				t.Fatal(err)
			}
			if err := fixture.coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			coordinator, err := storage.OpenSQLiteCoordinator(ctx, dbPath, storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			fixture.coordinator, fixture.repos = coordinator, storage.NewRepositories(coordinator.DB())
			gateway.createIssueCommentErr, gateway.updateIssueCommentErr = nil, nil
			runner = newRunner()
			// Scheduler discovery runs before the retry claim after restart.
			if _, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
				t.Fatal(err)
			}
			fixture.advance(time.Hour)
			claim, err = fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "test-fixer-restarted", "fixer")
			if err != nil || claim == nil {
				t.Fatalf("publication retry was dropped: %#v, %v", claim, err)
			}
			replayed, err := runner.ProcessClaimedItem(ctx, *claim)
			if err != nil || replayed.Status != "success" {
				t.Fatalf("publication replay = %#v, %v", replayed, err)
			}
			if len(agent.starts) != 1 || validations != 1 || len(git.pushCalls) != 1 || len(gateway.resolveNativeCalls) != 0 {
				t.Fatalf("replay repeated repair/validation/push/resolve: %d/%d/%d/%d", len(agent.starts), validations, len(git.pushCalls), len(gateway.resolveNativeCalls))
			}
			if len(gateway.createIssueComments) != tc.wantCreates || len(gateway.detail.IssueComments) != 1 || !strings.Contains(gateway.detail.IssueComments[0]["body"].(string), "code fixed; comment remains open") {
				t.Fatalf("publication did not converge to one truthful comment: creates=%d comments=%#v", len(gateway.createIssueComments), gateway.detail.IssueComments)
			}
			loop, _ = fixture.repos.Loops.GetByID(ctx, failed.LoopID)
			entry, ok := findThreadFixEvidence(loadFixEvidenceStoreV2(loop.MetadataJSON), item)
			if !ok || entry.ResolveState != forgejoFixedUnresolvedStatus || entry.Source != "agent_repair_result" {
				t.Fatalf("published decision not acknowledged: %#v", entry)
			}
			for poll := 0; poll < 3; poll++ {
				discovered, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
				if err != nil || len(discovered.QueueItems) != 0 {
					t.Fatalf("completed publication rediscovered work: %#v, %v", discovered, err)
				}
			}
		})
	}
}

func TestForgejoMixedRoundPublishesBeforeRediscoveringNewFeedback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                   string
		newNative, editedNative, newLegacy     bool
		publicationFails, responseLost, manual bool
	}{
		{name: "new native", newNative: true},
		{name: "edited native", editedNative: true},
		{name: "new legacy round", newLegacy: true},
		{name: "manual new legacy round", newLegacy: true, manual: true},
		{name: "new native and legacy", newNative: true, newLegacy: true},
		{name: "503 new native", newNative: true, publicationFails: true},
		{name: "lost response new native", newNative: true, publicationFails: true, responseLost: true},
		{name: "503 new native and legacy", newNative: true, newLegacy: true, publicationFails: true},
		{name: "lost response new native and legacy", newNative: true, newLegacy: true, publicationFails: true, responseLost: true},
		{name: "503 edited native", editedNative: true, publicationFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRunnerFixture(t)
			finding := ownNativeFinding(101, "u1")
			original := []NativeReviewComment{finding}
			if tc.editedNative {
				original = append(original, ownNativeFinding(103, "u1"))
			}
			gateway := &nativeLifecycleGateway{probe: forge.ProbeStateUnsupported, acceptFailedCreate: tc.responseLost, detail: forgejoDiscoveryDetail(t, "head-1", 1), fakeGitHubGateway: fakeGitHubGateway{currentUser: "looper", listOpen: []PullRequestSummary{{Number: 42, State: "OPEN", HeadSHA: "head-1", Author: "looper"}}, nativeComments: original}}
			if tc.publicationFails {
				gateway.createIssueCommentErr = &forge.ForgejoHTTPError{StatusCode: 503, Method: "POST", Path: "/comments", Message: "temporary outage"}
			}
			git := &nativeLifecycleGit{remote: gateway, fakeGitGateway: fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "feature/fix", HeadSHA: "head-1"}, prepareResult: PrepareWorktreeResult{HeadSHA: "head-1", Clean: true}, inspectResults: []InspectHeadResult{{HeadSHA: "head-1"}, {HeadSHA: "fixed-head", NewCommitSHAs: []string{"fixed-head"}}, {HeadSHA: "fixed-head"}}}}
			git.afterPush = func() {
				if tc.newNative {
					gateway.nativeComments = append([]NativeReviewComment{finding}, ownNativeFinding(102, "u2"))
				}
				if tc.editedNative {
					gateway.nativeComments = []NativeReviewComment{finding, ownNativeFinding(103, "u2")}
				}
				if tc.newLegacy {
					live := forgejoDiscoveryDetailWithItems(t, "fixed-head", 2, []forge.ReviewItem{
						{ReviewItemID: "R-001", Status: forge.ReviewItemStatusOpen, Title: "Different parser failure", Body: "New feedback reuses the stable ID in a new review round.", LastSeenRoundID: 2},
						{ReviewItemID: "R-002", Status: forge.ReviewItemStatusOpen, Title: "Check overflow", Body: "Reject values outside the supported range.", LastSeenRoundID: 2},
					})
					gateway.detail.IssueComments = live.IssueComments
				}
			}
			result := nativeAgentResult(original, "fixed")
			var payload map[string]any
			if err := json.Unmarshal([]byte(extractCompletionMarkerPayload(result.Stdout)), &payload); err != nil {
				t.Fatal(err)
			}
			payload["review_thread_replies"] = []map[string]any{{"fixItemId": "R-001", "action": "fixed", "explanation": "The parser now fails fast."}}
			result.Stdout = "__LOOPER_RESULT__=" + mustMarshalJSON(payload)
			agent := &fakeAgentExecutor{results: []AgentResult{result}}
			validations := 0
			newRunner := func() *Runner {
				runner := nativeLifecycleRunner(t, fixture, gateway, git, agent)
				runner.projectRoleConfig.Roles.Fixer.AutoDiscovery = !tc.manual
				runner.validationRunner = func(ctx context.Context, input ValidationInput) (ValidationResult, error) {
					validations++
					return passValidation(ctx, input)
				}
				return runner
			}
			runner := newRunner()
			// Seed a queue item, then exercise the same manual-loop contract as
			// the API with automatic discovery disabled for the entire execution.
			runner.projectRoleConfig.Roles.Fixer.AutoDiscovery = true
			discovered, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
			if err != nil || len(discovered.QueueItems) != 1 {
				t.Fatalf("initial discovery = %#v, %v", discovered, err)
			}
			runner.projectRoleConfig.Roles.Fixer.AutoDiscovery = !tc.manual
			if tc.manual {
				loop, err := fixture.repos.Loops.GetByID(ctx, *discovered.QueueItems[0].LoopID)
				if err != nil || loop == nil {
					t.Fatalf("manual loop = %#v, %v", loop, err)
				}
				if _, err := runner.mergeLoopMetadata(ctx, *loop, map[string]any{"manual": true}); err != nil {
					t.Fatal(err)
				}
			}
			fixture.advance(time.Hour)
			claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "mixed-fixer", "fixer")
			if err != nil || claim == nil {
				t.Fatalf("claim = %#v, %v", claim, err)
			}
			processed, err := runner.ProcessClaimedItem(ctx, *claim)
			if err != nil {
				t.Fatal(err)
			}
			run, _ := fixture.repos.Runs.GetByID(ctx, processed.RunID)
			checkpoint := parseCheckpoint(run.CheckpointJSON)
			loop, _ := fixture.repos.Loops.GetByID(ctx, processed.LoopID)
			fixedItem := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{finding}), nil, false)[0]
			if tc.publicationFails {
				if processed.Status != "failed" || checkpoint.ResumePolicy != loops.ResumePolicyReplayStep {
					t.Fatalf("publication failure = %#v, checkpoint = %#v", processed, checkpoint)
				}
				if evidence, ok := findThreadFixEvidence(loadFixEvidenceStoreV2(loop.MetadataJSON), fixedItem); ok && evidence.ResolveState == forgejoFixedUnresolvedStatus {
					t.Fatal("unpublished mixed result suppressed discovery")
				}
			}
			var sequence int
			var name, dbPath string
			if err := fixture.coordinator.DB().QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &dbPath); err != nil {
				t.Fatal(err)
			}
			if err := fixture.coordinator.Close(); err != nil {
				t.Fatal(err)
			}
			coordinator, err := storage.OpenSQLiteCoordinator(ctx, dbPath, storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			fixture.coordinator, fixture.repos = coordinator, storage.NewRepositories(coordinator.DB())
			runner = newRunner()
			if tc.publicationFails {
				gateway.createIssueCommentErr = nil
				if _, err := runner.DiscoverPullRequests(ctx, DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
					t.Fatal(err)
				}
				fixture.advance(time.Hour)
				claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "mixed-fixer-restarted", "fixer")
				if err != nil || claim == nil {
					t.Fatalf("publication retry dropped = %#v, %v", claim, err)
				}
				processed, err = runner.ProcessClaimedItem(ctx, *claim)
				if err != nil {
					t.Fatal(err)
				}
				run, _ = fixture.repos.Runs.GetByID(ctx, processed.RunID)
				checkpoint = parseCheckpoint(run.CheckpointJSON)
			}
			if tc.newNative || tc.editedNative {
				if processed.Status != "failed" || checkpoint.ResumePolicy != loops.ResumePolicyRestartFromDiscover {
					t.Fatalf("new native feedback did not restart discovery = %#v, %#v", processed, checkpoint)
				}
			} else if processed.Status != "success" {
				t.Fatalf("new legacy round should reach live recheck = %#v", processed)
			}
			if len(agent.starts) != 1 || validations != 1 || len(git.pushCalls) != 1 || len(gateway.resolveNativeCalls) != 0 {
				t.Fatalf("replayed agent/validation/push/resolve = %d/%d/%d/%d", len(agent.starts), validations, len(git.pushCalls), len(gateway.resolveNativeCalls))
			}
			loop, _ = fixture.repos.Loops.GetByID(ctx, processed.LoopID)
			if evidence, ok := findThreadFixEvidence(loadFixEvidenceStoreV2(loop.MetadataJSON), fixedItem); !ok || evidence.ResolveState != forgejoFixedUnresolvedStatus || evidence.ThreadFingerprint != finding.ObservedFingerprint {
				t.Fatalf("confirmed native result lost before rediscovery = %#v, %v", evidence, ok)
			}
			comments := forgeCommentsFromCheckpointDetail(pullRequestCheckpointDetail(gateway.detail))
			visible, summary, err := forge.ParseUniqueFixerSummaryComment(comments)
			if err != nil || summary.ConsumedReviewRoundID != 1 || len(summary.Results) != 1 || summary.Results[0].ReviewItemID != "R-001" || !strings.Contains(visible.Body, "code fixed; comment remains open") || len(gateway.detail.IssueComments) != 2 {
				t.Fatalf("mixed acknowledgement = %#v, %v, comments = %#v", summary, err, gateway.detail.IssueComments)
			}
			wantCreates := 1
			if tc.publicationFails && !tc.responseLost {
				wantCreates = 2
			}
			if len(gateway.createIssueComments) != wantCreates {
				t.Fatalf("created %d comments, want %d attempts", len(gateway.createIssueComments), wantCreates)
			}
			observed, ok, err := reviewerSummaryFromCheckpointDetail(checkpoint.Detail)
			if err != nil || !ok || observed.ReviewRoundID != 1 {
				t.Fatalf("agent-observed review snapshot was overwritten = %#v, %v", observed, err)
			}
			project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
			input := stepInput{Project: *project, Loop: *loop, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint}
			rechecked, err := runner.runRecheckStep(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			wantRemaining := map[string]bool{}
			if tc.newNative {
				wantRemaining[NativeReviewCommentFixItemID(102)] = true
			}
			if tc.editedNative {
				wantRemaining[NativeReviewCommentFixItemID(103)] = true
			}
			if tc.newLegacy {
				wantRemaining["R-001"], wantRemaining["R-002"] = true, true
			}
			assertRemaining := func(where string, items []FixItem) {
				t.Helper()
				if len(items) != len(wantRemaining) {
					t.Fatalf("%s remaining = %#v, want IDs %#v", where, items, wantRemaining)
				}
				for _, item := range items {
					if !wantRemaining[item.ID] {
						t.Fatalf("%s rediscovered acknowledged item = %#v", where, item)
					}
					if item.ID == "R-001" && !strings.Contains(item.Summary, "Different parser failure") {
						t.Fatalf("%s retained stale legacy input = %#v", where, item)
					}
				}
			}
			assertRemaining("recheck", rechecked.Recheck.RemainingFixItems)
			input.Checkpoint = fixerCheckpoint{}
			rediscovered, err := runner.runDiscoverPRStep(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			input.Checkpoint = rediscovered
			collected, err := runner.runCollectFixesStep(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			assertRemaining("fresh repair input", collected.FixItems)
		})
	}
}

func TestForgejoNativeSummaryIdentityFailureDoesNotCreateDuplicate(t *testing.T) {
	t.Parallel()
	gateway := &fakeGitHubGateway{currentUserErr: errors.New("HTTP 503 identity unavailable")}
	runner := New(Options{GitHub: gateway})
	item := nativeFixItem(101, "u1")
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "fixed-head", IssueComments: []map[string]any{{"id": int64(9000), "author": map[string]any{"login": "looper"}, "body": fixerRoundSummaryMarker("fixed-head")}}}, ResolvedComments: &checkpointResolvedComments{Items: []checkpointResolvedComment{{FixItemID: item.ID, Status: forgejoFixedUnresolvedStatus}}}}
	err := runner.publishRoundSummaryComment(context.Background(), stepInput{Repo: "acme/looper", PRNumber: 42}, &checkpoint, []FixItem{item}, "fixed-head", map[string]string{item.ID: "Fixed the calculation."})
	if err == nil || checkpoint.SummaryComment == nil || checkpoint.SummaryComment.State != "create_failed" || len(gateway.createIssueComments) != 0 || len(gateway.updateIssueComments) != 0 {
		t.Fatalf("identity failure publication = %#v, %v creates=%d updates=%d", checkpoint.SummaryComment, err, len(gateway.createIssueComments), len(gateway.updateIssueComments))
	}
}

func TestGitHubSummaryFailureRemainsBestEffort(t *testing.T) {
	t.Parallel()
	thread := ReviewThread{ID: "thread-1", Comments: []ReviewThreadComment{{ID: "comment-1", Author: "alice", Body: "Fix the calculation."}}}
	item := FixItem{ID: "comment-1", Type: "comment", ThreadID: "thread-1", Author: "alice"}
	gateway := &fakeGitHubGateway{createIssueCommentErr: errors.New("HTTP 503"), threads: []ReviewThread{thread}, viewResponses: []PullRequestDetail{{Number: 42, State: "OPEN", HeadSHA: "fixed-head", Comments: []map[string]any{{"id": item.ID, "threadId": item.ThreadID, "author": item.Author}}}}}
	checkpoint := fixerCheckpoint{
		Detail: &checkpointDetail{HeadSHA: "fixed-head"}, FixItems: []FixItem{item},
		Repair:           &checkpointRepair{ReplyExplanations: []replyExplanationEntry{{FixItemID: item.ID, ThreadID: item.ThreadID, Action: "fixed", Explanation: "Corrected the calculation.", ThreadCommentsObserved: hashReviewThreadComments(thread)}}},
		Validation:       &ValidationResult{Passed: true, HeadSHA: "fixed-head"},
		Push:             &checkpointPush{Pushed: true, HeadSHA: "fixed-head", Evidence: &fixEvidence{Valid: true, HeadSHA: "fixed-head", Source: "fallback_push", ProducedNewCommits: true}},
		ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "old-head", FinalHeadSHA: "fixed-head", NewCommitSHAs: []string{"fixed-head"}},
	}
	runner := New(Options{GitHub: gateway})
	updated, err := runner.runResolveCommentsStep(context.Background(), stepInput{Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
	if err != nil || updated.SummaryComment == nil || updated.SummaryComment.State != "create_failed" || len(gateway.resolveCalls) != 1 {
		t.Fatalf("GitHub best-effort summary changed: %#v, %v resolves=%d", updated.SummaryComment, err, len(gateway.resolveCalls))
	}
}

func TestForgejoNativeReplayDoesNotAcknowledgeEditedComment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRunnerFixture(t)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
	loop := storage.LoopRecord{ID: "native-replay", ProjectID: project.ID, Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	old := ownNativeFinding(101, "u1")
	newComment := ownNativeFinding(102, "u2")
	github := &nativeLifecycleGateway{probe: forge.ProbeStateUnsupported, detail: PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "fixed-head"}, fakeGitHubGateway: fakeGitHubGateway{currentUser: "looper", nativeComments: []NativeReviewComment{old, newComment}}}
	items := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{old}), nil, false)
	result := nativeAgentResult([]NativeReviewComment{old}, "fixed")
	checkpoint := fixerCheckpoint{Detail: pullRequestCheckpointDetail(github.detail), FixItems: items, Repair: &checkpointRepair{ReplyExplanations: parseNativeRepairResults(result.Stdout, "", items)}, Validation: &ValidationResult{Passed: true, HeadSHA: "fixed-head"}, Push: &checkpointPush{Pushed: true, HeadSHA: "fixed-head"}}
	runner := New(Options{Repos: fixture.repos, GitHub: github})
	input := stepInput{Project: *project, Loop: loop, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint}
	updated, err := runner.runResolveCommentsStep(ctx, input)
	if err == nil || !strings.Contains(err.Error(), "omitted") {
		t.Fatalf("first resolve = %v", err)
	}
	if updated.ResolvedComments.Items[0].Status != forgejoFixedUnresolvedStatus {
		t.Fatalf("partial success = %#v", updated.ResolvedComments)
	}
	// resolve refresh has overwritten the live FixItems, but the decision still
	// carries the old fingerprint. Editing that same ID must re-arm it.
	github.nativeComments = []NativeReviewComment{ownNativeFinding(101, "edited")}
	input.Checkpoint = parseCheckpoint(stringPtr(mustJSON(t, updated)))
	updated, err = runner.runResolveCommentsStep(ctx, input)
	if err == nil || !strings.Contains(err.Error(), "changed") || updated.ResolvedComments.Items[0].Status != "skipped_thread_drift" {
		t.Fatalf("edited replay = %#v, %v", updated.ResolvedComments, err)
	}
	metadata, _ := runner.freshNativeFixMetadata(ctx, &loop)
	live := normalizeFixItems(nativeReviewCommentsToMaps(github.nativeComments), nil, false)
	remaining, err := runner.suppressFixedForgejoNativeItems(ctx, *project, input.Repo, 42, "fixed-head", &storage.LoopRecord{MetadataJSON: metadata}, live)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("edited discovery = %#v, %v", remaining, err)
	}
}

func TestForgejoNativeProvenanceAndPushEvidenceIsolation(t *testing.T) {
	t.Parallel()
	base := ownNativeFinding(101, "u1")
	for _, tc := range []struct {
		name   string
		mutate func(*NativeReviewComment)
		want   bool
	}{
		{"own reviewer", func(*NativeReviewComment) {}, true},
		{"human reviewer", func(c *NativeReviewComment) { c.Author = "alice"; c.ReviewBody = "" }, true},
		{"own chatter", func(c *NativeReviewComment) { c.ReviewBody = "Just a note.\n<!-- looper:stamp -->" }, false},
		{"fixer reply", func(c *NativeReviewComment) { c.Author = "fix-bot"; c.Body = "<!-- looper:fixer-round head=head-1 -->" }, false},
		{"pending", func(c *NativeReviewComment) { c.ReviewState = "PENDING" }, false},
		{"dismissed", func(c *NativeReviewComment) { c.ReviewState = "DISMISSED" }, false},
		{"wrong parent", func(c *NativeReviewComment) { c.ReviewAuthor = "mallory" }, false},
		{"wrong head", func(c *NativeReviewComment) { c.ReviewCommitID = "other-head" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comment := base
			tc.mutate(&comment)
			if got := eligibleNativeReviewComment(comment, "looper"); got != tc.want {
				t.Fatalf("eligible = %v", got)
			}
		})
	}
	entry := threadFixEvidence{ThreadID: "comment", ThreadFingerprint: "fingerprint", EvidenceHeadSHA: "fixed-head", Source: "agent_repair_result", ResolveState: forgejoFixedUnresolvedStatus, Explanation: "Fixed."}
	store := upsertThreadFixEvidence(nil, entry)
	pending := entry
	pending.Source, pending.ResolveState = "fallback_push", "pending"
	store = upsertThreadFixEvidence(store, pending)
	if got := store.Threads[entry.ThreadID][0]; got.Source != entry.Source || got.ResolveState != entry.ResolveState || got.EvidenceHeadSHA != entry.EvidenceHeadSHA {
		t.Fatalf("same-head replay replaced decision: %#v", got)
	}
	pending.EvidenceHeadSHA = "unrelated-head"
	store = upsertThreadFixEvidence(store, pending)
	if got := store.Threads[entry.ThreadID][0]; got.ResolveState != "pending" {
		t.Fatalf("old decision inherited onto new head: %#v", got)
	}
}

func TestForgejoNativeNeedsHumanUsesSharedPreMutationSuspension(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRunnerFixture(t)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
	finding := ownNativeFinding(101, "u1")
	items := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{finding}), nil, false)
	result := nativeAgentResult([]NativeReviewComment{finding}, "needs_human")
	agent := &fakeAgentExecutor{results: []AgentResult{result}}
	github := &fakeGitHubGateway{}
	git := &fakeGitGateway{prepareResult: PrepareWorktreeResult{Clean: true}}
	runner := New(Options{Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, ValidationRunner: passValidation, HITLEnabled: true, CustomInstructions: forgejoFixerDiscoveryConfig(t, fixture)})
	root, err := config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := fixerCheckpoint{Detail: &checkpointDetail{HeadSHA: "head-1", HeadRefName: "feature/fix"}, FixItems: items, Worktree: &checkpointWorktree{Path: filepath.Join(root, "wt"), Branch: "feature/fix", HeadSHA: "head-1", BaseHeadSHA: "head-1", PreparedAt: fixture.nowISO()}}
	_, err = runner.runRepairStep(ctx, stepInput{Project: *project, Loop: storage.LoopRecord{MetadataJSON: stringPtr(`{"manual":true}`)}, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint})
	var awaiting *awaitingHumanError
	if !errors.As(err, &awaiting) {
		t.Fatalf("repair = %v, want shared human suspension", err)
	}
	if len(git.pushCalls) != 0 || len(git.commitCalls) != 0 || len(github.resolveNativeCalls) != 0 || len(github.createIssueComments) != 0 || len(github.dismissedReviews) != 0 {
		t.Fatal("needs_human mutated repository or reviews")
	}
	if len(agent.starts) != 1 || !strings.Contains(agent.starts[0].Prompt, "repair_results") || !strings.Contains(agent.starts[0].Prompt, "STOP THE ENTIRE TURN BEFORE MAKING EDITS") {
		t.Fatal("native human contract missing")
	}
}

func TestForgejoNativeMixedCollectionKeepsOnlyUnconsumedItemsAndCIContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRunnerFixture(t)
	project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
	detail := forgejoDiscoveryDetail(t, "head-1", 1)
	detail.URL = "https://forgejo.test/acme/looper/pulls/42"
	detail.Checks = []map[string]any{{"name": "unit tests", "state": "FAILURE", "description": "discount boundary failed", "url": "https://forgejo.test/acme/looper/actions/runs/5", "actionRunId": int64(901)}}
	detail.HasConflicts = true
	summary := forge.NewFixerSummary(1, 1, []forge.FixerResult{{ReviewItemID: "R-001", Result: forge.FixerItemResultFixed, Explanation: "Already repaired."}})
	summary.ObservedHeadSHA = "head-1"
	marker, err := forge.RenderFixerSummary(summary)
	if err != nil {
		t.Fatal(err)
	}
	detail.IssueComments = append(detail.IssueComments, map[string]any{"id": int64(2), "author": map[string]any{"login": "looper"}, "body": marker})
	github := &fakeGitHubGateway{nativeComments: []NativeReviewComment{ownNativeFinding(101, "u1")}}
	cfg := forgejoFixerDiscoveryConfig(t, fixture)
	runner := New(Options{GitHub: github, Repos: fixture.repos, CustomInstructions: cfg})
	checkpoint, err := runner.runCollectFixesStep(ctx, stepInput{Project: *project, Loop: storage.LoopRecord{}, Repo: "acme/looper", PRNumber: 42, Checkpoint: fixerCheckpoint{Detail: pullRequestCheckpointDetail(detail)}})
	if err != nil || len(checkpoint.FixItems) != 3 {
		t.Fatalf("collect = %#v, %v", checkpoint.FixItems, err)
	}
	for _, item := range checkpoint.FixItems {
		if item.Source == "forgejo-reviewer-summary" {
			t.Fatalf("consumed item returned: %#v", item)
		}
	}
	prompt, _ := buildFixerPrompt(project.ID, *cfg, "acme/looper", 42, checkpoint.Detail, checkpoint.FixItems, true, config.DefaultDisclosureConfig(), "codex", "")
	for _, want := range []string{detail.URL, "FORGEJO_TOKEN", "discount boundary failed", "https://forgejo.test/acme/looper/actions/runs/5", `"actionRunId":901`, "Agent-side Forgejo fetch contract"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "Agent-side GitHub fetch contract") {
		t.Fatal("Forgejo prompt uses GitHub fetch instructions")
	}
}

func TestForgejoLegacyFixerMessageUsesCommonTemplateAndPreservesProtocol(t *testing.T) {
	t.Parallel()
	summary := forge.NewFixerSummary(2, 3, []forge.FixerResult{{ReviewItemID: "R-001", Result: forge.FixerItemResultFixed, Explanation: "Corrected the calculation."}})
	summary.ObservedHeadSHA = "fixed-head"
	body, err := renderForgejoFixerSummaryComment(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "**Looper fixer round complete**") || strings.Contains(body, "Reviewer Summary") || strings.Contains(body, "Forgejo Fixer Summary") || strings.Contains(body, "Consumed") {
		t.Fatalf("legacy body = %q", body)
	}
	_, parsed, err := forge.ParseUniqueFixerSummaryComment([]forge.Comment{{ID: 1, Body: body}})
	if err != nil || parsed.ConsumedReviewRoundID != 3 || parsed.FixRoundID != 2 {
		t.Fatalf("hidden handoff = %#v, %v", parsed, err)
	}
	detail := &checkpointDetail{IssueComments: []map[string]any{{"id": int64(1), "author": map[string]any{"login": "looper"}, "body": body}}}
	if id, _ := findExistingFixerSummaryCommentID(detail, "fixed-head", "looper"); id != 0 {
		t.Fatal("native updater may erase legacy handoff")
	}
}

func TestForgejoNativeUnfixedDecisionsRemainOpenAndReachExistingNoProgressHold(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"fixed", "declined", "deferred"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRunnerFixture(t)
			project, _ := fixture.repos.Projects.GetByID(ctx, "project_1")
			loop := storage.LoopRecord{ID: "no-push", ProjectID: project.ID, Type: "fixer", TargetType: "pull_request", Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatal(err)
			}
			comment := ownNativeFinding(101, "u1")
			github := &nativeLifecycleGateway{probe: forge.ProbeStateUnsupported, detail: PullRequestDetail{Number: 42, State: "OPEN", HeadSHA: "fixed-head"}, fakeGitHubGateway: fakeGitHubGateway{nativeComments: []NativeReviewComment{comment}}}
			items := normalizeFixItems(nativeReviewCommentsToMaps([]NativeReviewComment{comment}), nil, false)
			result := nativeAgentResult([]NativeReviewComment{comment}, action)
			checkpoint := fixerCheckpoint{Detail: pullRequestCheckpointDetail(github.detail), FixItems: items, Repair: &checkpointRepair{ReplyExplanations: parseNativeRepairResults(result.Stdout, "", items)}, Validation: &ValidationResult{Passed: true, HeadSHA: "fixed-head"}, Push: &checkpointPush{Pushed: false, SkippedReason: "No new commits to push"}, ReconcileCommits: &checkpointReconcileCommits{BaseHeadSHA: "fixed-head", FinalHeadSHA: "fixed-head", WorkingTreeClean: true}}
			runner := New(Options{Repos: fixture.repos, GitHub: github, CustomInstructions: forgejoFixerDiscoveryConfig(t, fixture)})
			input := stepInput{Project: *project, Loop: loop, Repo: "acme/looper", PRNumber: 42, Checkpoint: checkpoint}
			updated, err := runner.runResolveCommentsStep(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			input.Checkpoint = updated
			checked, err := runner.runRecheckStep(ctx, input)
			if action == "fixed" {
				if err != nil || len(checked.Recheck.RemainingFixItems) != 0 {
					t.Fatalf("already-fixed no-push recheck = %#v, %v", checked.Recheck, err)
				}
			} else {
				var hold *loopError
				if !errors.As(err, &hold) || hold.kind != FailureManualIntervention || len(checked.Recheck.RemainingFixItems) != 1 {
					t.Fatalf("unfixed recheck = %#v, %v", checked.Recheck, err)
				}
				metadata, _ := runner.freshNativeFixMetadata(ctx, &loop)
				if entry, ok := findThreadFixEvidence(loadFixEvidenceStoreV2(metadata), items[0]); ok && entry.ResolveState == forgejoFixedUnresolvedStatus {
					t.Fatal("unfixed decision recorded as fixed")
				}
			}
			if len(github.resolveNativeCalls) != 0 || len(github.createIssueComments) != 1 {
				t.Fatalf("resolve/comments = %d/%d", len(github.resolveNativeCalls), len(github.createIssueComments))
			}
			input.Checkpoint = updated
			if _, err := runner.runResolveCommentsStep(ctx, input); err != nil {
				t.Fatal(err)
			}
			if len(github.createIssueComments) != 1 {
				t.Fatal("replay posted duplicate summary")
			}
		})
	}
}
