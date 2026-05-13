package e2e

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/e2e/harness"
	internalfixer "github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/storage"
)

func TestScenarioResolveCommentsRefreshesPRHeadAfterPush(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	originPath := harness.CreateBareOrigin(t, "git", repo.Path)
	featureHead := harness.CreateBranchCommitAndPush(t, "git", repo.Path, "feature/fix-42", "fix-target.txt", "needs fix\n")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, loadGHSchemaFixture(t))
	fakeGH.WriteState(t, harness.GHState{
		CurrentUserLogin: "looper",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {
				Number:           42,
				Repo:             "acme/looper",
				Title:            "Fix review feedback",
				Author:           "looper",
				State:            "OPEN",
				HeadRefName:      "feature/fix-42",
				BaseRefName:      "main",
				HeadRef:          "refs/heads/feature/fix-42",
				BaseRef:          "refs/heads/main",
				GitDir:           originPath,
				MergeStateStatus: "CLEAN",
				Threads: []harness.GHThread{{
					ID:         "thread-1",
					IsResolved: false,
					Path:       "fix-target.txt",
					Line:       1,
					Comments: []harness.GHThreadComment{{
						ID:                "comment-1",
						Body:              "please fix this",
						Author:            "alice",
						Path:              "fix-target.txt",
						Line:              1,
						CommitOID:         featureHead,
						OriginalCommitOID: featureHead,
						URL:               "https://example.test/thread-1",
					}},
				}},
			},
		},
	})
	cfg := fixerConfigWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port, "commit-with-review-replies")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{"projectId": "project_1", "type": "fixer", "targetType": "pull_request", "repo": "acme/looper", "prNumber": 42}, &created)
	run := waitForRunTerminal(t, client, created.ID, 60*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success (error=%v checkpoint=%v)", run.Status, run.ErrorMessage, run.CheckpointJSON)
	}
	loop := loadSingleLoop(t, client, created.ID)
	loopMeta := parseJSONObject(t, loop.MetadataJSON)
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	push, _ := checkpoint["push"].(map[string]any)
	if push == nil || push["pushed"] != true {
		t.Fatalf("checkpoint.push = %#v, want pushed=true (run=%#v checkpoint=%#v loopMeta=%#v)", push, run, checkpoint, loopMeta)
	}
	pushedHead, _ := push["headSha"].(string)
	if pushedHead == "" || pushedHead == featureHead {
		t.Fatalf("push.headSha = %q, want pushed head newer than %s (loopMeta=%#v checkpoint=%#v)", pushedHead, featureHead, loopMeta, checkpoint)
	}
	resolvedComments, _ := checkpoint["resolvedComments"].(map[string]any)
	items, _ := resolvedComments["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("resolved comments items = %#v, want one resolved thread", resolvedComments)
	}
	state := loadFakeGHStateFile(t, fakeGH.StatePath)
	pr := state.PullRequests["acme/looper#42"]
	if len(pr.Threads) != 1 || !pr.Threads[0].IsResolved {
		t.Fatalf("thread state = %#v, want resolved thread", pr.Threads)
	}
	if len(pr.Threads[0].Comments) < 2 {
		t.Fatalf("thread comments = %#v, want original comment plus Looper reply", pr.Threads[0].Comments)
	}
	if !strings.Contains(pr.Threads[0].Comments[len(pr.Threads[0].Comments)-1].Body, pushedHead[:7]) {
		t.Fatalf("latest thread reply = %#v, want pushed head prefix %q", pr.Threads[0].Comments[len(pr.Threads[0].Comments)-1], pushedHead[:7])
	}
	invocations := readInvocationLog(t, fakeGH.InvocationLog)
	assertInvocationContainsOrdered(t, invocations, []string{"pr", "view", "42", "--repo", "acme/looper"})
	assertInvocationContainsOrdered(t, invocations, []string{"api", "graphql"})
	assertInvocationContainsSubstring(t, invocations, "resolveReviewThread")
	proc.Stop(context.Background())
}

func TestScenarioResolveCommentsSkipsClosedPullRequest(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, loadGHSchemaFixture(t))
	fakeGH.WriteState(t, harness.GHState{
		CurrentUserLogin: "looper",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {
				Number:         42,
				Repo:           "acme/looper",
				Title:          "Already closed",
				Author:         "looper",
				State:          "CLOSED",
				HeadRefName:    "feature/fix-42",
				BaseRefName:    "main",
				HeadSHA:        repo.InitialCommit,
				BaseSHA:        repo.InitialCommit,
				ReviewDecision: "",
			},
		},
	})
	cfg := fixerConfigWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port, "success-no-diff")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{"projectId": "project_1", "type": "fixer", "targetType": "pull_request", "repo": "acme/looper", "prNumber": 42}, &created)
	run := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success skip (error=%v checkpoint=%v)", run.Status, run.ErrorMessage, run.CheckpointJSON)
	}
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	skipReason, _ := checkpoint["skipReason"].(string)
	if !strings.Contains(skipReason, "not eligible") {
		t.Fatalf("skipReason = %q, want not-eligible closed-PR skip", skipReason)
	}
	if _, err := os.Stat(fakeAgent.EvidencePath()); !os.IsNotExist(err) {
		t.Fatalf("fake agent evidence = %v, want no agent execution for closed PR", err)
	}
	proc.Stop(context.Background())
}

func TestScenarioResolveCommentsSkipsWhenNoNewCommitLeavesThreadsUnresolved(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	originPath := harness.CreateBareOrigin(t, "git", repo.Path)
	featureHead := harness.CreateBranchCommitAndPush(t, "git", repo.Path, "feature/fix-42", "fix-target.txt", "needs fix\n")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, loadGHSchemaFixture(t))
	fakeGH.WriteState(t, harness.GHState{
		CurrentUserLogin: "looper",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {
				Number:           42,
				Repo:             "acme/looper",
				Title:            "No-op fix attempt",
				Author:           "looper",
				State:            "OPEN",
				HeadRefName:      "feature/fix-42",
				BaseRefName:      "main",
				HeadRef:          "refs/heads/feature/fix-42",
				BaseRef:          "refs/heads/main",
				GitDir:           originPath,
				MergeStateStatus: "CLEAN",
				Threads: []harness.GHThread{{
					ID:         "thread-1",
					IsResolved: false,
					Path:       "fix-target.txt",
					Line:       1,
					Comments: []harness.GHThreadComment{{
						ID:                "comment-1",
						Body:              "please fix this",
						Author:            "alice",
						Path:              "fix-target.txt",
						Line:              1,
						CommitOID:         featureHead,
						OriginalCommitOID: featureHead,
						URL:               "https://example.test/thread-1",
					}},
				}},
			},
		},
	})
	cfg := fixerConfigWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port, "success-no-diff")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{"projectId": "project_1", "type": "fixer", "targetType": "pull_request", "repo": "acme/looper", "prNumber": 42}, &created)
	run := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success no-op resolve path (error=%v checkpoint=%v)", run.Status, run.ErrorMessage, run.CheckpointJSON)
	}
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	if got, _ := checkpoint["resumePolicy"].(string); got != "advance_from_checkpoint" {
		t.Fatalf("resumePolicy = %q, want advance_from_checkpoint", got)
	}
	push, _ := checkpoint["push"].(map[string]any)
	if push == nil || push["pushed"] != false {
		t.Fatalf("checkpoint.push = %#v, want pushed=false", push)
	}
	if reason, _ := push["skippedReason"].(string); !strings.Contains(reason, "No new commits") {
		t.Fatalf("push.skippedReason = %q, want no-new-commits skip", reason)
	}
	resolvedComments, _ := checkpoint["resolvedComments"].(map[string]any)
	items, _ := resolvedComments["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("resolvedComments.items = %#v, want one skipped item", resolvedComments)
	}
	first, _ := items[0].(map[string]any)
	if first["status"] != "agent_declined" {
		t.Fatalf("resolvedComments item = %#v, want agent_declined", first)
	}
	loop := loadSingleLoop(t, client, created.ID)
	if loop.Status != "completed" {
		t.Fatalf("loop status = %s, want completed after no-op resolve", loop.Status)
	}
	state := loadFakeGHStateFile(t, fakeGH.StatePath)
	pr := state.PullRequests["acme/looper#42"]
	if len(pr.Threads) != 1 || pr.Threads[0].IsResolved {
		t.Fatalf("thread state = %#v, want unresolved thread preserved", pr.Threads)
	}
	proc.Stop(context.Background())
}

func TestScenarioResolveCommentsIgnoresStaleNoPushMetadataAndLeavesThreadsUnresolved(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	originPath := harness.CreateBareOrigin(t, "git", repo.Path)
	featureHead := harness.CreateBranchCommitAndPush(t, "git", repo.Path, "feature/fix-42", "fix-target.txt", "needs fix\n")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, loadGHSchemaFixture(t))
	fakeGH.WriteState(t, harness.GHState{
		CurrentUserLogin: "looper",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {
				Number:           42,
				Repo:             "acme/looper",
				Title:            "Stale no-push rerun",
				Author:           "looper",
				State:            "OPEN",
				HeadRefName:      "feature/fix-42",
				BaseRefName:      "main",
				HeadRef:          "refs/heads/feature/fix-42",
				BaseRef:          "refs/heads/main",
				GitDir:           originPath,
				MergeStateStatus: "CLEAN",
				Threads: []harness.GHThread{{
					ID:         "thread-1",
					IsResolved: false,
					Path:       "fix-target.txt",
					Line:       1,
					Comments: []harness.GHThreadComment{{
						ID:                "comment-1",
						Body:              "please fix this",
						Author:            "alice",
						Path:              "fix-target.txt",
						Line:              1,
						CommitOID:         featureHead,
						OriginalCommitOID: featureHead,
						URL:               "https://example.test/thread-1",
					}},
				}},
			},
		},
	})
	cfg := fixerConfigWithFakeTools(t, bins, home, repo, fakeGH, fakeAgent, port, "success-no-diff")
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	fixItemsHash := hashFixItemsForTest([]internalfixer.FixItem{{Type: "comment", ID: "comment-1", ThreadID: "thread-1", Summary: "please fix this", Author: "alice", URL: "https://example.test/thread-1", Path: "fix-target.txt", Line: 1}})
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{
		"projectId":  "project_1",
		"type":       "fixer",
		"targetType": "pull_request",
		"repo":       "acme/looper",
		"prNumber":   42,
		"status":     "paused",
		"metadata": map[string]any{
			"lastFixHeadSha":   "stale-head",
			"lastFixItemsHash": fixItemsHash,
		},
	}, &created)
	var started struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops/"+created.ID+"/start", nil, &started)
	run := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success for stale no-push metadata (error=%v checkpoint=%v)", run.Status, run.ErrorMessage, run.CheckpointJSON)
	}
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	push, _ := checkpoint["push"].(map[string]any)
	if push == nil || push["pushed"] != false {
		t.Fatalf("checkpoint.push = %#v, want pushed=false", push)
	}
	if reason, _ := push["skippedReason"].(string); !strings.Contains(reason, "No new commits") {
		t.Fatalf("push.skippedReason = %q, want no-new-commits skip", reason)
	}
	resolvedComments, _ := checkpoint["resolvedComments"].(map[string]any)
	items, _ := resolvedComments["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("resolvedComments.items = %#v, want one skipped item", resolvedComments)
	}
	first, _ := items[0].(map[string]any)
	if first["status"] != "agent_declined" {
		t.Fatalf("resolvedComments item = %#v, want agent_declined", first)
	}
	loop := loadSingleLoop(t, client, created.ID)
	if loop.Status != "completed" {
		t.Fatalf("loop status = %s, want completed after stale no-push metadata", loop.Status)
	}
	state := loadFakeGHStateFile(t, fakeGH.StatePath)
	pr := state.PullRequests["acme/looper#42"]
	if len(pr.Threads) != 1 || pr.Threads[0].IsResolved {
		t.Fatalf("thread state = %#v, want unresolved thread preserved", pr.Threads)
	}
	proc.Stop(context.Background())
}

func TestScenarioWorkerNoDiffBranchDoesNotCreatePullRequest(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	_ = harness.CreateBareOrigin(t, "git", repo.Path)
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, loadGHSchemaFixture(t))
	fakeGH.WriteState(t, harness.GHState{CurrentUserLogin: "looper"})
	vendor, command, agentEnv := fakeAgent.AgentConfig("success-no-diff", "git")
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.AllowAutoCommit = true
	cfg.Defaults.AllowAutoPush = true
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyAllDone
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/workers", map[string]any{"projectId": "project_1", "prompt": "do not change anything", "repo": "acme/looper", "baseBranch": "main"}, &created)
	run := waitForRunTerminal(t, client, created.ID, 30*time.Second)
	if run.Status != "success" {
		t.Fatalf("run status = %s, want success skip (error=%q checkpoint=%s)", run.Status, stringValue(run.ErrorMessage), stringValue(run.CheckpointJSON))
	}
	checkpoint := parseJSONObject(t, run.CheckpointJSON)
	skipReason, _ := checkpoint["skipReason"].(string)
	if !strings.Contains(skipReason, "has no commits ahead of main") {
		t.Fatalf("skipReason = %q, want no-ahead/no-diff branch skip", skipReason)
	}
	if checkpoint["pullRequest"] != nil {
		t.Fatalf("pullRequest = %#v, want no PR created", checkpoint["pullRequest"])
	}
	invocations := readInvocationLog(t, fakeGH.InvocationLog)
	assertInvocationContainsSubstring(t, invocations, "repos/acme/looper/compare/main...")
	assertNoInvocationStartsWith(t, invocations, []string{"pr", "create"})
	proc.Stop(context.Background())
}

func TestScenarioResumedFixerStopsWhenPullRequestCloses(t *testing.T) {
	bins := harness.MustBinaries(t)
	home := harness.NewTempHome(t)
	repo := harness.CreateSeededRepo(t, "git")
	port := harness.MustFreePort(t)
	fakeAgent := harness.NewFakeAgent(t, bins)
	fakeGH := harness.NewFakeGH(t, bins, loadGHSchemaFixture(t))
	fakeGH.WriteState(t, harness.GHState{
		CurrentUserLogin: "looper",
		PullRequests: map[string]harness.GHPullRequest{
			"acme/looper#42": {
				Number:      42,
				Repo:        "acme/looper",
				Title:       "Closed PR",
				Author:      "looper",
				State:       "CLOSED",
				HeadRefName: "feature/fix-42",
				BaseRefName: "main",
				HeadSHA:     repo.InitialCommit,
				BaseSHA:     repo.InitialCommit,
			},
		},
	})
	vendor, command, agentEnv := fakeAgent.AgentConfig("success-no-diff", "git")
	cfg := harness.DefaultConfig(t, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.AllowAutoCommit = true
	cfg.Defaults.AllowAutoPush = true
	cfg.Defaults.AllowRiskyFixes = true
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyManual
	harness.WriteConfig(t, home.ConfigPath, cfg, nil)
	proc := harness.StartLooperd(t, bins, home, home.ConfigPath, fakeGH.EnvMap(), cfg.Server.Host, cfg.Server.Port)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := proc.WaitForReady(ctx); err != nil {
		t.Fatalf("wait for ready: %v", err)
	}
	client := newAPIClient(proc.BaseURL())
	var created struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops", map[string]any{"projectId": "project_1", "type": "fixer", "targetType": "pull_request", "repo": "acme/looper", "prNumber": 42, "status": "paused"}, &created)
	_, repos := openRepos(t, home.DBPath)
	checkpointJSON := mustMarshal(t, map[string]any{
		"claimedLockKey": "pr:acme/looper:42",
		"detail": map[string]any{
			"state":       "CLOSED",
			"headSha":     repo.InitialCommit,
			"headRefName": "feature/fix-42",
			"baseRefName": "main",
			"baseSha":     repo.InitialCommit,
		},
	})
	if err := repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_failed_resume_pr_closed", LoopID: created.ID, Status: "failed", LastCompletedStep: stringPtr("claim-pr"), CheckpointJSON: &checkpointJSON, StartedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), UpdatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	var started struct {
		ID string `json:"id"`
	}
	client.post(t, "/api/v1/loops/"+created.ID+"/start", nil, &started)
	second := waitForNewTerminalRun(t, client, created.ID, map[string]struct{}{"run_failed_resume_pr_closed": {}}, 30*time.Second)
	if second.Status != "success" {
		t.Fatalf("second run status = %s, want skipped success (error=%q checkpoint=%s)", second.Status, stringValue(second.ErrorMessage), stringValue(second.CheckpointJSON))
	}
	checkpoint := parseJSONObject(t, second.CheckpointJSON)
	skipReason, _ := checkpoint["skipReason"].(string)
	if !strings.Contains(skipReason, "not eligible") {
		t.Fatalf("skipReason = %q, want closed-PR resumed skip", skipReason)
	}
	if _, err := os.Stat(fakeAgent.EvidencePath()); !os.IsNotExist(err) {
		t.Fatalf("fake agent evidence = %v, want no resumed fixer agent execution for closed target", err)
	}
	proc.Stop(context.Background())
}

func fixerConfigWithFakeTools(tb testing.TB, bins harness.BuiltBinaries, home harness.TempHome, repo harness.SeededRepo, fakeGH harness.FakeGH, fakeAgent harness.FakeAgent, port int, agentMode string) config.Config {
	tb.Helper()
	vendor, command, agentEnv := fakeAgent.AgentConfig(agentMode, "git")
	cfg := harness.DefaultConfig(tb, home, harness.ConfigOptions{
		Port:              port,
		ToolPaths:         harness.TestToolPaths{Git: "git", GH: fakeGH.Path, Looper: bins.LooperPath, Osascript: bins.FakeOsascriptPath},
		EnableOsascript:   true,
		AgentVendor:       vendor,
		AgentCommand:      command,
		AgentEnv:          agentEnv,
		Projects:          writeProjectConfig(repo, home),
		DisableDisclosure: true,
	})
	cfg.Scheduler.PollIntervalSeconds = 10
	cfg.Defaults.AllowAutoCommit = true
	cfg.Defaults.AllowAutoPush = true
	cfg.Defaults.AllowRiskyFixes = true
	cfg.Defaults.OpenPRStrategy = config.OpenPRStrategyManual
	return cfg
}

func loadGHSchemaFixture(tb testing.TB) harness.GHSchema {
	tb.Helper()
	payload, err := os.ReadFile(filepath.Join("githubcontract", "testdata", "gh-schema", "schema.json"))
	if err != nil {
		tb.Fatalf("read gh schema fixture: %v", err)
	}
	var schema harness.GHSchema
	if err := json.Unmarshal(payload, &schema); err != nil {
		tb.Fatalf("decode gh schema fixture: %v", err)
	}
	return schema
}

func loadFakeGHStateFile(tb testing.TB, path string) harness.GHState {
	tb.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read fake gh state: %v", err)
	}
	var state harness.GHState
	if err := json.Unmarshal(payload, &state); err != nil {
		tb.Fatalf("decode fake gh state: %v", err)
	}
	return state
}

func assertInvocationContainsOrdered(tb testing.TB, invocations []map[string]any, want []string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv, _ := invocation["argv"].([]any)
		parts := make([]string, 0, len(argv))
		for _, part := range argv {
			parts = append(parts, part.(string))
		}
		if containsOrdered(parts, want) {
			return
		}
	}
	tb.Fatalf("did not find invocation containing %v in %#v", want, invocations)
}

func containsOrdered(haystack []string, needle []string) bool {
	if len(needle) == 0 {
		return true
	}
	start := 0
	for _, want := range needle {
		found := false
		for start < len(haystack) {
			if haystack[start] == want {
				found = true
				start++
				break
			}
			start++
		}
		if !found {
			return false
		}
	}
	return true
}

func assertNoInvocationStartsWith(tb testing.TB, invocations []map[string]any, wantPrefix []string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv, _ := invocation["argv"].([]any)
		parts := make([]string, 0, len(argv))
		for _, part := range argv {
			parts = append(parts, part.(string))
		}
		if len(parts) < len(wantPrefix) {
			continue
		}
		match := true
		for i := range wantPrefix {
			if parts[i] != wantPrefix[i] {
				match = false
				break
			}
		}
		if match {
			tb.Fatalf("unexpected invocation with prefix %v: %v", wantPrefix, parts)
		}
	}
}

func assertInvocationContainsSubstring(tb testing.TB, invocations []map[string]any, want string) {
	tb.Helper()
	for _, invocation := range invocations {
		argv, _ := invocation["argv"].([]any)
		for _, part := range argv {
			if strings.Contains(part.(string), want) {
				return
			}
		}
	}
	tb.Fatalf("did not find invocation containing substring %q in %#v", want, invocations)
}

func hashFixItemsForTest(items []internalfixer.FixItem) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			panic(err)
		}
		parts = append(parts, string(encoded))
	}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func waitForRunTerminalCount(tb testing.TB, client apiClient, loopID string, count int, timeout time.Duration) runView {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var runs runsListResponse
		client.get(tb, "/api/v1/runs?loopId="+loopID, &runs)
		if len(runs.Items) >= count {
			last := runs.Items[len(runs.Items)-1]
			switch last.Status {
			case "success", "failed", "cancelled", "stopped":
				return runView(last)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for loop %s terminal run count %d", loopID, count)
	panic("unreachable")
}

func waitForNewTerminalRun(tb testing.TB, client apiClient, loopID string, existing map[string]struct{}, timeout time.Duration) runView {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var runs runsListResponse
		client.get(tb, "/api/v1/runs?loopId="+loopID, &runs)
		for _, item := range runs.Items {
			if _, ok := existing[item.ID]; ok {
				continue
			}
			switch item.Status {
			case "success", "failed", "cancelled", "stopped":
				return runView(item)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for new terminal run for loop %s", loopID)
	panic("unreachable")
}
