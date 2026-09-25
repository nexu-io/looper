package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func coverageMustFix() reviewerCommentOnlyFindingResult {
	return reviewerCommentOnlyFindingResult{
		Title: "Bug", Body: "Nil deref", Disposition: reviewFindingDispositionMustFix, Severity: reviewFindingSeverityBlocking,
		ScopeBasis: reviewFindingScopeIntroducedRegression, ScopeEvidence: "new path",
	}
}

func TestRecoveredCoverageDoesNotChangeMarkerRequirements(t *testing.T) {
	t.Parallel()
	completion := reviewerCommentOnlyCompletion{Summary: "Bug", Outcome: "blocking", Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()}}
	var requirements []bool
	for _, coverage := range []*reviewerCoverageReport{nil, {PassKind: "first_pass", Incomplete: []string{"unreviewed.go"}}} {
		completion.Coverage = coverage
		payload, err := json.Marshal(completion)
		if err != nil {
			t.Fatal(err)
		}
		summary := recoveredReviewerSummaryJSON(AgentResult{Stdout: "__LOOPER_RESULT__=" + string(payload)})
		requirements = append(requirements, pendingNativeMustFixRequiresActionableMarker(pendingReviewCheckpoint{ReviewerSummaryJSON: summary}))
	}
	if requirements[0] != requirements[1] {
		t.Fatalf("adding advisory coverage changed recovered marker requirements: %v", requirements)
	}
}

func TestValidateReviewerCommentOnlyCompletionCoverage(t *testing.T) {
	t.Parallel()

	oldJSON := `{"summary":"Must fix one","outcome":"blocking","findings":[{"title":"Bug","body":"Nil deref","disposition":"must_fix","severity":"blocking","scopeBasis":"introduced_regression","scopeEvidence":"new path"}]}`
	var old reviewerCommentOnlyCompletion
	if err := json.Unmarshal([]byte(oldJSON), &old); err != nil {
		t.Fatalf("unmarshal old completion: %v", err)
	}
	got, err := validateReviewerCommentOnlyCompletion(old)
	if err != nil {
		t.Fatalf("old JSON without coverage: %v", err)
	}
	if got.Coverage != nil {
		t.Fatalf("missing coverage = %#v, want unknown nil", got.Coverage)
	}

	withCoverage, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{
			PassKind:          "first_pass",
			ScopeBasis:        "changed_ranges",
			Reviewed:          []string{"internal/reviewer/runner.go"},
			Incomplete:        []string{"internal/cliapp/app.go"},
			IncompleteReasons: []string{"budget exhausted before cliapp"},
		},
	})
	if err != nil {
		t.Fatalf("findings + incomplete: %v", err)
	}
	if len(withCoverage.Findings) != 1 {
		t.Fatalf("findings dropped: %#v", withCoverage.Findings)
	}
	if withCoverage.Coverage == nil || withCoverage.Coverage.PassKind != "first_pass" || len(withCoverage.Coverage.Incomplete) != 1 {
		t.Fatalf("coverage = %#v", withCoverage.Coverage)
	}

	invalidKind, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{PassKind: "full_manifest"},
	})
	if err != nil {
		t.Fatalf("invalid passKind must not fail completion: %v", err)
	}
	if len(invalidKind.Findings) != 1 {
		t.Fatalf("invalid passKind dropped findings: %#v", invalidKind.Findings)
	}
	if invalidKind.Coverage != nil {
		t.Fatalf("invalid passKind coverage = %#v, want nil", invalidKind.Coverage)
	}

	repair, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{PassKind: "repair_frontier", Incomplete: []string{"delta only"}},
	})
	if err != nil {
		t.Fatalf("repair_frontier coverage: %v", err)
	}
	if repair.Coverage == nil || repair.Coverage.PassKind != "repair_frontier" {
		t.Fatalf("repair coverage = %#v", repair.Coverage)
	}

	missingKind, err := validateReviewerCommentOnlyCompletion(reviewerCommentOnlyCompletion{
		Summary:  "Must fix one",
		Outcome:  "blocking",
		Findings: []reviewerCommentOnlyFindingResult{coverageMustFix()},
		Coverage: &reviewerCoverageReport{Reviewed: []string{"a.go"}},
	})
	if err != nil {
		t.Fatalf("missing passKind must not fail completion: %v", err)
	}
	if len(missingKind.Findings) != 1 {
		t.Fatalf("missing passKind dropped findings: %#v", missingKind.Findings)
	}
	if missingKind.Coverage != nil {
		t.Fatalf("missing passKind coverage = %#v, want nil", missingKind.Coverage)
	}
}

func TestBuildReviewPromptIncludesCoverageContract(t *testing.T) {
	t.Parallel()

	prompt := buildReviewPrompt("acme/looper", 42, reviewerCheckpoint{Snapshot: &checkpointSnapshot{HeadSHA: "abc123"}}, "run_1", "reviewer:loop:abc123", config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment, Blocking: config.ReviewerReviewEventComment}, false, config.ReviewerScopeChangedRanges, config.DefaultDisclosureConfig(), "opencode", "", "/opt/looper/bin/looper")
	for _, want := range []string{
		"You MAY include optional `coverage`",
		"Missing coverage means unknown, not complete",
		"Coverage never changes publish eligibility",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseReviewerNativeCompletionKeepsCleanCoverage(t *testing.T) {
	t.Parallel()
	got, err := parseReviewerNativeCompletion(AgentResult{
		Stdout:  `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[],"coverage":{"passKind":"first_pass","incomplete":["internal/cliapp/app.go"],"incompleteReasons":["budget"]}}`,
		Summary: "No actionable findings",
	})
	if err != nil {
		t.Fatalf("parse native clean coverage: %v", err)
	}
	if len(got.Findings) != 0 || got.Coverage == nil || got.Coverage.PassKind != "first_pass" || len(got.Coverage.Incomplete) != 1 {
		t.Fatalf("native clean coverage = %#v", got)
	}

	dropped, err := parseReviewerNativeCompletion(AgentResult{
		Stdout:  `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[],"coverage":{"reviewed":["a.go"]}}`,
		Summary: "No actionable findings",
	})
	if err != nil {
		t.Fatalf("parse native missing passKind: %v", err)
	}
	if dropped.Coverage != nil {
		t.Fatalf("missing passKind coverage = %#v, want nil", dropped.Coverage)
	}
}

func TestParseReviewerCompletionDropsMalformedCoverage(t *testing.T) {
	t.Parallel()

	commentOnly, err := parseReviewerCommentOnlyCompletion(AgentResult{
		Stdout: `__LOOPER_RESULT__={"summary":"Must fix one","outcome":"blocking","findings":[{"title":"Bug","body":"Nil deref","disposition":"must_fix","severity":"blocking","scopeBasis":"introduced_regression","scopeEvidence":"new path"}],"coverage":{"passKind":"first_pass","reviewed":"internal/reviewer/runner.go"}}`,
	})
	if err != nil {
		t.Fatalf("malformed coverage must not reject comment-only completion: %v", err)
	}
	if len(commentOnly.Findings) != 1 {
		t.Fatalf("comment-only findings = %#v, want kept", commentOnly.Findings)
	}
	if commentOnly.Coverage != nil {
		t.Fatalf("comment-only coverage = %#v, want dropped", commentOnly.Coverage)
	}

	native, err := parseReviewerNativeCompletion(AgentResult{
		Stdout:  `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[],"coverage":{"passKind":"first_pass","reviewed":"a.go"}}`,
		Summary: "No actionable findings",
	})
	if err != nil {
		t.Fatalf("malformed coverage must not reject native completion: %v", err)
	}
	if native.Summary != "No actionable findings" {
		t.Fatalf("native summary = %q", native.Summary)
	}
	if native.Coverage != nil {
		t.Fatalf("native coverage = %#v, want dropped", native.Coverage)
	}
}

func TestRunReviewStepPersistsCleanNativeCoverage(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	agent := &fakeAgentExecutor{results: []AgentResult{{
		Status:      "completed",
		Summary:     "No actionable findings",
		ParseStatus: "parsed",
		Stdout:      `__LOOPER_RESULT__={"summary":"No actionable findings","outcome":"clean","findings":[],"coverage":{"passKind":"first_pass","incomplete":["internal/cliapp/app.go"],"incompleteReasons":["budget"]}}`,
	}}}
	runner := New(Options{
		DB:            fixture.coordinator.DB(),
		Repos:         fixture.repos,
		GitHub:        &fakeGitHubGateway{},
		Git:           &fakeGitGateway{},
		AgentExecutor: agent,
		Logger:        fixture.logger,
		Now:           fixture.now,
		ReviewEvents:  config.ReviewerReviewEventsConfig{Clean: config.ReviewerReviewEventComment},
	})
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil || project == nil {
		t.Fatalf("Projects.GetByID() = (%#v, %v), want project", project, err)
	}
	checkpoint, err := runner.runReviewStep(context.Background(), stepInput{
		Project:  *project,
		Loop:     storage.LoopRecord{ID: "loop_1"},
		Run:      storage.RunRecord{ID: "run_1"},
		Repo:     "acme/looper",
		PRNumber: 42,
		Checkpoint: reviewerCheckpoint{
			Detail:   &checkpointDetail{HeadRefName: "feature/review-me", BaseRefName: "main"},
			Snapshot: &checkpointSnapshot{HeadSHA: "abc123"},
			Worktree: &checkpointWorktree{Path: t.TempDir(), Branch: "feature/review-me", PreparedAt: fixture.nowISO()},
		},
	})
	if err != nil {
		t.Fatalf("runReviewStep() error = %v", err)
	}
	if checkpoint.PendingReview == nil || !strings.Contains(checkpoint.PendingReview.ReviewerSummaryJSON, `"passKind":"first_pass"`) {
		t.Fatalf("pending = %#v, want persisted coverage for clean native review", checkpoint.PendingReview)
	}
}

func TestProcessClaimedItemRetainsCoverageAcrossRecoveryAndScopePark(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       string
		parseStatus  string
		needsHuman   bool
		publishMode  config.ReviewerPublishMode
		findingsJSON string
	}{
		{name: "failed native marker recovery", status: "failed", parseStatus: "parsed"},
		{name: "unparsed native marker recovery", status: "completed", parseStatus: "failed"},
		{name: "failed recovery with legacy finding", status: "failed", parseStatus: "parsed", findingsJSON: `[{"title":"Legacy finding"}]`},
		{name: "unparsed recovery with malformed findings", status: "completed", parseStatus: "failed", findingsJSON: `{"legacy":"findings format"}`},
		{name: "native scope park", status: "completed", parseStatus: "parsed", needsHuman: true},
		{name: "comment-only scope park", status: "completed", parseStatus: "parsed", needsHuman: true, publishMode: config.ReviewerPublishModeSummaryComment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := newRunnerFixture(t)
			ctx := context.Background()
			completion := reviewerCommentOnlyCompletion{
				Summary: "No actionable findings", Outcome: "clean",
				Coverage: &reviewerCoverageReport{PassKind: "first_pass", Incomplete: []string{"unreviewed.go"}, IncompleteReasons: []string{"budget exhausted"}},
			}
			if tc.needsHuman {
				completion.Summary, completion.Outcome = "Need human", "blocking"
				completion.Findings = []reviewerCommentOnlyFindingResult{{
					Title: "Ambiguous scope", Body: "Clarify the requested behavior", Disposition: "needs_human", Severity: "blocking",
					ScopeBasis: "ambiguous_intent", ScopeEvidence: "PR non-goals", Path: "a.go", Line: 1,
				}}
			}
			payload, err := json.Marshal(completion)
			if err != nil {
				t.Fatal(err)
			}
			if tc.findingsJSON != "" {
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(payload, &envelope); err != nil {
					t.Fatal(err)
				}
				envelope["findings"] = json.RawMessage(tc.findingsJSON)
				payload, err = json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
			}
			github := &fakeGitHubGateway{reviewRequests: []string{"octocat"}, reviewMarkerMissing: tc.needsHuman, reviewMarkerOutcome: "clean", reviewMarkerEvent: ReviewEventComment}
			agent := &fakeAgentExecutor{results: []AgentResult{{Status: tc.status, Summary: completion.Summary, ParseStatus: tc.parseStatus, Stdout: "__LOOPER_RESULT__=" + string(payload)}}}
			cfg, err := config.DefaultConfig(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg.HITL.Enabled = false
			cfg.Roles.Reviewer.Behavior.ReviewEvents.Clean = config.ReviewerReviewEventComment
			if tc.publishMode != "" {
				cfg.Roles.Reviewer.Behavior.PublishMode = tc.publishMode
			}
			runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, CustomInstructions: &cfg, LoopConfig: testReviewerLoopConfig()})
			repo, prNumber := "acme/looper", int64(42)
			metadata := `{"followUpdates":true,"loop":{"enabled":true}}`
			loop := storage.LoopRecord{ID: "coverage_lifecycle", Seq: 1, ProjectID: "project_1", Type: "reviewer", TargetType: "pull_request", Repo: &repo, PRNumber: &prNumber, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
			if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
				t.Fatal(err)
			}
			if _, err := runner.enqueue(ctx, enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: repo, PRNumber: prNumber}); err != nil {
				t.Fatal(err)
			}
			claim, err := fixture.repos.Queue.ClaimNextOfType(ctx, fixture.nowISO(), "coverage-worker", "reviewer")
			if err != nil || claim == nil {
				t.Fatalf("ClaimNextOfType() = (%#v, %v)", claim, err)
			}
			result, err := runner.ProcessClaimedItem(ctx, *claim)
			if err != nil {
				t.Fatal(err)
			}
			var persisted reviewerCommentOnlyCompletion
			if tc.needsHuman {
				updated, err := fixture.repos.Loops.GetByID(ctx, loop.ID)
				if err != nil || updated == nil || !loops.IsReviewScopeHumanHold(*updated) || result.Status != "skipped" {
					t.Fatalf("scope park = (%#v, %v), result = %#v", updated, err, result)
				}
				evidence := loops.ReadReviewScopeHumanState(updated.MetadataJSON).Evidence
				if err := json.Unmarshal([]byte(evidence), &persisted); err != nil {
					t.Fatalf("parked evidence did not preserve the completion: %v; %s", err, evidence)
				}
				if len(persisted.Findings) != 1 || persisted.Findings[0].Disposition != "needs_human" {
					t.Fatalf("parked findings = %#v", persisted.Findings)
				}
			} else {
				if result.Status != "success" {
					t.Fatalf("recovered result = %#v", result)
				}
				run, err := fixture.repos.Runs.GetByID(ctx, result.RunID)
				if err != nil || run == nil {
					t.Fatalf("Runs.GetByID() = (%#v, %v)", run, err)
				}
				pending := parseCheckpoint(run.CheckpointJSON).PendingReview
				if pending == nil {
					t.Fatal("recovered run has no pending review checkpoint")
				}
				if err := json.Unmarshal([]byte(pending.ReviewerSummaryJSON), &persisted); err != nil {
					t.Fatalf("recovered completion was not persisted: %v", err)
				}
			}
			if persisted.Coverage == nil || len(persisted.Coverage.Incomplete) != 1 || persisted.Coverage.Incomplete[0] != "unreviewed.go" {
				t.Fatalf("persisted coverage = %#v", persisted.Coverage)
			}
		})
	}
}
