package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/fixer"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/reviewer"
)

// TestForgejoLiveProviderContracts is a transport/adapter test, not a reviewer
// policy bypass or an agent E2E. It creates two PRs and three unique branches.
// The extra merge opt-in permits protection of, and immediate merge into, only
// its own base branch. Existing reviewer policy is covered by offline tests.
//
// Required: LOOPER_FORGEJO_LIVE_CONTRACTS=1, LOOPER_FORGEJO_LIVE_BASE_URL,
// LOOPER_FORGEJO_LIVE_REPO, LOOPER_FORGEJO_LIVE_TEA_LOGIN.
// Optional: LOOPER_FORGEJO_LIVE_TEA_PATH, LOOPER_FORGEJO_LIVE_MERGE=1.
// Run with -v -count=1 -timeout=15m to retain resource IDs and cleanup proof.
// Closed PRs and statuses on test commits remain as audit history.
func TestForgejoLiveProviderContracts(t *testing.T) {
	if os.Getenv("LOOPER_FORGEJO_LIVE_CONTRACTS") != "1" {
		t.Skip("set LOOPER_FORGEJO_LIVE_CONTRACTS=1 and explicit Forgejo target/login to create live test resources")
	}
	required := func(name string) string {
		t.Helper()
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("%s is required for live writes", name)
		}
		return value
	}
	baseURL := strings.TrimRight(required("LOOPER_FORGEJO_LIVE_BASE_URL"), "/")
	repo := required("LOOPER_FORGEJO_LIVE_REPO")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || url.PathEscape(repo) != strings.ReplaceAll(repo, "/", "%2F") || strings.Contains(repo, "..") {
		t.Fatal("LOOPER_FORGEJO_LIVE_REPO must be an explicit owner/repository slug")
	}
	login := required("LOOPER_FORGEJO_LIVE_TEA_LOGIN")
	teaPath := strings.TrimSpace(os.Getenv("LOOPER_FORGEJO_LIVE_TEA_PATH"))
	if teaPath == "" {
		teaPath = "tea"
	}
	teaPath, err := exec.LookPath(teaPath)
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	f := &forgejoLiveFixture{t: t, ctx: ctx, repo: repo, tea: teaPath, login: login, gitPath: gitPath,
		prefix: "looper-provider-contract-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(nonce[:]),
		dir:    t.TempDir(), merge: os.Getenv("LOOPER_FORGEJO_LIVE_MERGE") == "1"}
	f.base, f.clean, f.conflict = f.prefix+"-base", f.prefix+"-clean", f.prefix+"-conflict"
	source, caller := filepath.Join(f.dir, "source"), filepath.Join(f.dir, "caller")
	cfg := config.Config{
		Tools:     config.ToolPathsConfig{GitPath: &gitPath},
		Providers: []config.ProviderConfig{{ID: "live", Kind: config.ProviderKindForgejo, BaseURL: baseURL, Auth: config.ProviderAuthTea, TeaLogin: &login, TeaPath: &teaPath}},
		Projects:  []config.ProjectRefConfig{{ID: "live", Provider: "live", Repo: repo, RepoPath: caller}},
	}
	client, ok, err := forgejoClientForRepo(&cfg, repo)
	if err != nil || !ok {
		t.Fatalf("configured Forgejo tea provider: matched=%v err=%v", ok, err)
	}
	actor, err := client.CurrentUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.actor = actor.Login
	var metadata struct {
		FullName      string                           `json:"full_name"`
		SSHURL        string                           `json:"ssh_url"`
		DefaultBranch string                           `json:"default_branch"`
		AllowMerge    bool                             `json:"allow_merge_commits"`
		AllowSquash   bool                             `json:"allow_squash_merge"`
		AllowRebase   bool                             `json:"allow_rebase"`
		Permissions   struct{ Admin, Push, Pull bool } `json:"permissions"`
	}
	f.mustAPI(http.MethodGet, "", nil, &metadata, http.StatusOK)
	if metadata.FullName != repo || !metadata.Permissions.Push || !metadata.Permissions.Pull || (f.merge && !metadata.Permissions.Admin) {
		t.Fatalf("target/permissions unsuitable: repo=%q permissions=%+v merge=%v", metadata.FullName, metadata.Permissions, f.merge)
	}
	if err := forgejoLiveValidateSSHURL(metadata.SSHURL, repo); err != nil {
		t.Fatal(err)
	}
	if metadata.DefaultBranch == "" {
		t.Fatal("repository has no existing default branch; this test never seeds a repository")
	}
	f.unchanged = map[string]string{metadata.DefaultBranch: f.branch(metadata.DefaultBranch).Commit.ID}
	if metadata.DefaultBranch != "main" {
		var main forgejoLiveBranch
		status, err := f.api(ctx, http.MethodGet, "/branches/main", nil, &main)
		if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
			t.Fatalf("read main baseline: HTTP %d, %v", status, err)
		}
		if status == http.StatusOK {
			f.unchanged["main"] = main.Commit.ID
		}
	}
	for branch, sha := range f.unchanged {
		if sha == "" {
			t.Fatalf("empty baseline SHA for %s", branch)
		}
		t.Logf("baseline repo=%s branch=%s sha=%s actor=%s prefix=%s", repo, branch, sha, actor.Login, f.prefix)
	}
	for _, branch := range []string{f.base, f.clean, f.conflict} {
		f.mustAPI(http.MethodGet, "/branches/"+url.PathEscape(branch), nil, nil, http.StatusNotFound)
	}
	f.mustAPI(http.MethodGet, "/branch_protections/"+url.PathEscape(f.base), nil, nil, http.StatusNotFound)
	// Register before any write so a lost creation response is still cleaned up.
	t.Cleanup(f.cleanup)
	for _, path := range []string{source, caller} {
		f.runGit(f.dir, "init", "-b", "local-caller", path)
		f.runGit(path, "config", "user.name", "Looper Provider Contract")
		f.runGit(path, "config", "user.email", "looper-provider-contract@example.invalid")
		f.runGit(path, "config", "commit.gpgsign", "false")
		f.runGit(path, "config", "core.hooksPath", filepath.Join(f.dir, "no-hooks"))
		f.runGit(path, "remote", "add", "origin", metadata.SSHURL)
		f.runGit(path, "fetch", "--no-tags", "origin", "refs/heads/"+metadata.DefaultBranch+":refs/remotes/origin/baseline")
		f.runGit(path, "checkout", "-B", "local-caller", "refs/remotes/origin/baseline")
		if actual := f.runGit(path, "rev-parse", "HEAD"); actual != f.unchanged[metadata.DefaultBranch] {
			t.Fatalf("default branch changed during fixture preparation: %s", actual)
		}
	}
	// The adapters must preserve a dirty caller, including its refs and FETCH_HEAD.
	forgejoMergeTestWriteFile(t, filepath.Join(caller, "README.md"), "local staged work\n")
	f.runGit(caller, "add", "README.md")
	forgejoMergeTestWriteFile(t, filepath.Join(caller, "README.md"), "local unstaged work\n")
	forgejoMergeTestWriteFile(t, filepath.Join(caller, "untracked.txt"), "local untracked work\n")
	forgejoMergeTestWriteFile(t, filepath.Join(caller, ".git", "FETCH_HEAD"), "local fetch marker\n")
	before := mergeConflictCallerSnapshot(t, caller)
	assertCaller := func() {
		t.Helper()
		if after := mergeConflictCallerSnapshot(t, caller); before != after {
			t.Fatal("Forgejo adapter changed caller branch, refs, index, worktree, or FETCH_HEAD")
		}
		if _, err := os.Stat(filepath.Join(caller, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
			t.Fatalf("caller merge state changed: %v", err)
		}
	}
	fixtureFile := f.prefix + ".txt"
	f.runGit(source, "checkout", "-b", f.base)
	initial := f.commit(source, fixtureFile, "original\n")
	f.runGit(source, "checkout", "-b", f.conflict, initial)
	conflictSHA := f.commit(source, fixtureFile, "conflicting head\n")
	f.runGit(source, "checkout", f.base)
	baseSHA := f.commit(source, fixtureFile, "advanced base\n")
	f.runGit(source, "checkout", "-b", f.clean)
	cleanSHA := f.commit(source, f.prefix+"-clean.txt", "clean head\n")
	for _, branch := range []string{f.base, f.conflict, f.clean} {
		f.push(source, branch)
	}
	createPR := func(head string) int64 {
		t.Helper()
		pr, err := client.CreatePullRequest(ctx, forge.CreatePullRequestInput{Title: "Looper provider contract: " + strings.TrimPrefix(head, f.prefix+"-"), Body: "Temporary live provider contract fixture. Only its unique test base may be merged; no reviewer policy assertion or production branch mutation.", Head: head, Base: f.base})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("created PR=%d url=%s base=%s head=%s", pr.Number, pr.HTMLURL, f.base, head)
		return pr.Number
	}
	conflictPR, cleanPR := createPR(f.conflict), createPR(f.clean)
	reviewAdapter, fixAdapter := reviewerGitHubAdapter{config: &cfg}, fixerGitHubAdapter{config: &cfg}
	for _, tc := range []struct {
		number int64
		sha    string
		want   bool
	}{{conflictPR, conflictSHA, true}, {cleanPR, cleanSHA, false}} {
		f.await("PR head/base and conflict state", func() bool {
			pr, err := reviewAdapter.ViewPullRequest(ctx, reviewer.ViewPullRequestInput{Repo: repo, PRNumber: tc.number, CWD: caller})
			if err != nil {
				t.Fatal(err)
			}
			return pr.HeadSHA == tc.sha && pr.BaseSHA == baseSHA && pr.HasConflicts == tc.want
		})
		fix, err := fixAdapter.ViewPullRequest(ctx, fixer.ViewPullRequestInput{Repo: repo, PRNumber: tc.number, CWD: caller})
		if err != nil || fix.HeadSHA != tc.sha || fix.BaseSHA != baseSHA || fix.HasConflicts != tc.want {
			t.Fatalf("fixer PR %d: detail=%+v err=%v", tc.number, fix, err)
		}
		assertCaller()
		t.Logf("adapters PR=%d head=%s base=%s HasConflicts=%v caller unchanged", tc.number, tc.sha, baseSHA, tc.want)
	}
	settings, err := reviewAdapter.GetRepositorySettings(ctx, githubinfra.RepositorySettingsInput{Repo: repo})
	if err != nil || !settings.AllowAutoMerge || settings.AllowMergeCommit != metadata.AllowMerge || settings.AllowSquashMerge != metadata.AllowSquash || settings.AllowRebaseMerge != metadata.AllowRebase {
		t.Fatalf("repository merge settings=%+v err=%v", settings, err)
	}
	assertProtection := func(branch string) {
		t.Helper()
		raw := f.branch(branch)
		got, err := reviewAdapter.GetBranchProtection(ctx, githubinfra.BranchProtectionInput{Repo: repo, Branch: branch})
		if err != nil || got.Enabled != raw.Protected || got.HasRequiredChecks != (raw.EnableStatusCheck && len(raw.StatusCheckContexts) > 0) {
			t.Fatalf("effective branch protection %s: adapter=%+v raw=%+v err=%v", branch, got, raw, err)
		}
		t.Logf("branch protection branch=%s protected=%v required_contexts=%v", branch, raw.Protected, raw.StatusCheckContexts)
	}
	assertProtection(metadata.DefaultBranch)
	assertProtection(f.base)
	checkName := f.prefix + "/required"
	checkURL := baseURL + "/" + repo + "/pulls/" + strconv.FormatInt(cleanPR, 10)
	statusID := f.status(cleanSHA, checkName, "failure", "intentional live contract failure", checkURL)
	assertStatus := func(sha, state, description string, id int64) {
		t.Helper()
		checks, err := client.ListCommitChecks(ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, check := range checks {
			if check.Name == checkName {
				count++
				if check.ID != id || check.State != state || check.Description != description || check.URL != checkURL {
					t.Fatalf("current check lost latest state/diagnostics: %+v", check)
				}
			}
		}
		if count != 1 {
			t.Fatalf("current context count=%d, want one after status deduplication", count)
		}
		fix, err := fixAdapter.ViewPullRequest(ctx, fixer.ViewPullRequestInput{Repo: repo, PRNumber: cleanPR, CWD: caller})
		if err != nil || fix.HeadSHA != sha {
			t.Fatalf("fixer current head=%s err=%v", fix.HeadSHA, err)
		}
		count = 0
		for _, check := range fix.Checks {
			if check["name"] == checkName {
				count++
				if check["state"] != state || check["description"] != description || check["url"] != checkURL {
					t.Fatalf("fixer check diagnostics=%+v", check)
				}
			}
		}
		if count != 1 {
			t.Fatalf("fixer context count=%d, want one", count)
		}
		review, err := reviewAdapter.ViewPullRequest(ctx, reviewer.ViewPullRequestInput{Repo: repo, PRNumber: cleanPR, CWD: caller})
		if err != nil || review.HeadSHA != sha || !strings.Contains(review.ChecksSummary, state) || review.URL != checkURL || fix.URL != checkURL {
			t.Fatalf("reviewer check/URL mapping: head=%s checks=%q URL=%q err=%v", review.HeadSHA, review.ChecksSummary, review.URL, err)
		}
		assertCaller()
	}
	assertStatus(cleanSHA, "FAILURE", "intentional live contract failure", statusID)
	merge := githubinfra.EnableAutoMergeInput{Repo: repo, PRNumber: cleanPR, Strategy: config.ReviewerAutoMergeStrategyMerge, HeadSHA: cleanSHA}
	requireMergeStatus := func(want int) {
		t.Helper()
		err := reviewAdapter.EnableAutoMerge(ctx, merge)
		var httpErr *forge.ForgejoHTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != want {
			t.Fatalf("immediate merge expected head=%s: err=%v, want HTTP %d", merge.HeadSHA, err, want)
		}
		if actual := f.branch(f.base).Commit.ID; actual != baseSHA {
			t.Fatalf("rejected merge changed own base: got %s want %s", actual, baseSHA)
		}
		t.Logf("immediate merge rejected PR=%d expected_head=%s HTTP=%d own base unchanged", cleanPR, merge.HeadSHA, want)
	}
	if f.merge {
		if !settings.AllowMergeCommit || f.branch(f.base).Protected {
			t.Fatal("merge fixture requires merge commits and an initially unprotected unique base")
		}
		f.protection = true
		f.mustAPI(http.MethodPost, "/branch_protections", map[string]any{"rule_name": f.base, "apply_to_admins": true, "enable_push": true, "enable_status_check": true, "status_check_contexts": []string{checkName}}, nil, http.StatusCreated)
		raw := f.branch(f.base)
		if !raw.Protected || !raw.EnableStatusCheck || !reflect.DeepEqual(raw.StatusCheckContexts, []string{checkName}) {
			t.Fatalf("own required CI protection not effective: %+v", raw)
		}
		assertProtection(f.base)
		requireMergeStatus(http.StatusMethodNotAllowed) // Scheduling would return 201 and fail here.
	}
	successID := f.status(cleanSHA, checkName, "success", "live contract passed", checkURL)
	if successID <= statusID {
		t.Fatalf("status history IDs did not advance: %d -> %d", statusID, successID)
	}
	assertStatus(cleanSHA, "SUCCESS", "live contract passed", successID)
	var history []struct {
		ID      int64  `json:"id"`
		Context string `json:"context"`
	}
	f.mustAPI(http.MethodGet, "/statuses/"+cleanSHA+"?limit=50&page=1&sort=highestindex", nil, &history, http.StatusOK)
	found := map[int64]bool{}
	for _, status := range history {
		if status.Context == checkName {
			found[status.ID] = true
		}
	}
	if !found[statusID] || !found[successID] {
		t.Fatal("live status history does not contain both failure and success")
	}
	if !f.merge {
		t.Log("immediate merge contract skipped; set LOOPER_FORGEJO_LIVE_MERGE=1 as an additional explicit opt-in")
		return
	}
	merge.HeadSHA = baseSHA // Wrong existing commit, with required CI now successful.
	requireMergeStatus(http.StatusConflict)
	newHead := f.commit(source, f.prefix+"-clean.txt", "separately authorized new head\n")
	f.push(source, f.clean)
	f.await("updated PR head", func() bool {
		pr, err := client.ViewPullRequest(ctx, cleanPR)
		if err != nil {
			t.Fatal(err)
		}
		return pr.Head.SHA == newHead
	})
	newStatusID := f.status(newHead, checkName, "success", "new head live contract passed", checkURL)
	assertStatus(newHead, "SUCCESS", "new head live contract passed", newStatusID)
	merge.HeadSHA = cleanSHA // The previously authorized head must not authorize this push.
	requireMergeStatus(http.StatusConflict)
	// This direct transport call is authorized by the second live-test opt-in
	// for this exact new fixture commit. It does not assert reviewer eligibility.
	merge.HeadSHA = newHead
	if err := reviewAdapter.EnableAutoMerge(ctx, merge); err != nil {
		t.Fatal(err)
	}
	var merged forgejoLivePR
	f.mustAPI(http.MethodGet, fmt.Sprintf("/pulls/%d", cleanPR), nil, &merged, http.StatusOK)
	if !merged.Merged || merged.State != "closed" || merged.Base.Ref != f.base || merged.Head.SHA != newHead {
		t.Fatalf("immediate merge result=%+v", merged)
	}
	mergedBase := f.branch(f.base).Commit.ID
	f.runGit(source, "fetch", "--no-tags", "--no-write-fetch-head", "origin", mergedBase)
	f.runGit(source, "merge-base", "--is-ancestor", newHead, mergedBase)
	assertCaller()
	t.Logf("merged only own base PR=%d reviewed_head=%s base=%s before=%s after=%s", cleanPR, newHead, f.base, baseSHA, mergedBase)
}

type forgejoLiveBranch struct {
	Name                string   `json:"name"`
	Protected           bool     `json:"protected"`
	EnableStatusCheck   bool     `json:"enable_status_check"`
	StatusCheckContexts []string `json:"status_check_contexts"`
	Commit              struct {
		ID string `json:"id"`
	} `json:"commit"`
}

type forgejoLivePR struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct{ Ref, SHA string } `json:"base"`
	Head struct{ Ref, SHA string } `json:"head"`
}

type forgejoLiveFixture struct {
	t                                             *testing.T
	ctx                                           context.Context
	repo, tea, login, gitPath, actor, prefix, dir string
	base, clean, conflict                         string
	merge, protection                             bool
	unchanged                                     map[string]string
}

// Fixture-only REST setup/cleanup. All assertions exercise the production
// client/adapters. tea holds credentials; neither config files nor tokens are read.
func (f *forgejoLiveFixture) api(ctx context.Context, method, suffix string, payload, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"api", "--login", f.login, "-i", "-X", method, "/repos/" + f.repo + suffix}
	var input []byte
	if payload != nil {
		var err error
		input, err = json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		args = append(args, "-d", "@-")
	}
	cmd := exec.CommandContext(ctx, f.tea, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	// tea can exit zero on HTTP errors. Require an actual final status line.
	status := 0
	for _, line := range strings.Split(stderr.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "HTTP/") {
			status, _ = strconv.Atoi(fields[1])
		}
	}
	if err != nil || status < 100 {
		return status, fmt.Errorf("tea fixture %s %s: process=%v HTTP=%d", method, suffix, err, status)
	}
	if out != nil && status >= 200 && status < 300 {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return status, fmt.Errorf("tea fixture %s %s returned invalid JSON: %w", method, suffix, err)
		}
	}
	return status, nil
}

func (f *forgejoLiveFixture) mustAPI(method, suffix string, payload, out any, want int) {
	f.t.Helper()
	status, err := f.api(f.ctx, method, suffix, payload, out)
	if err != nil || status != want {
		f.t.Fatalf("fixture %s %s: HTTP=%d err=%v, want %d", method, suffix, status, err, want)
	}
}

func (f *forgejoLiveFixture) branch(name string) forgejoLiveBranch {
	f.t.Helper()
	var branch forgejoLiveBranch
	f.mustAPI(http.MethodGet, "/branches/"+url.PathEscape(name), nil, &branch, http.StatusOK)
	return branch
}

func (f *forgejoLiveFixture) runGit(cwd string, args ...string) string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.gitPath, args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("fixture git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *forgejoLiveFixture) commit(cwd, name, body string) string {
	f.t.Helper()
	forgejoMergeTestWriteFile(f.t, filepath.Join(cwd, name), body)
	f.runGit(cwd, "add", "--", name)
	f.runGit(cwd, "commit", "-m", "test: "+f.prefix)
	return f.runGit(cwd, "rev-parse", "HEAD")
}

func (f *forgejoLiveFixture) push(cwd, branch string) {
	f.t.Helper()
	if branch != f.base && branch != f.clean && branch != f.conflict {
		f.t.Fatalf("refusing fixture push outside exact owned branches: %s", branch)
	}
	f.runGit(cwd, "push", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	f.t.Logf("pushed own branch=%s sha=%s", branch, f.runGit(cwd, "rev-parse", branch))
}

func (f *forgejoLiveFixture) status(sha, name, state, description, targetURL string) int64 {
	f.t.Helper()
	var status struct {
		ID int64 `json:"id"`
	}
	f.mustAPI(http.MethodPost, "/statuses/"+sha, map[string]string{"context": name, "state": state, "description": description, "target_url": targetURL}, &status, http.StatusCreated)
	f.t.Logf("created status id=%d sha=%s context=%s state=%s", status.ID, sha, name, state)
	return status.ID
}

func (f *forgejoLiveFixture) await(what string, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		select {
		case <-f.ctx.Done():
			f.t.Fatal(f.ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func (f *forgejoLiveFixture) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	call := func(method, path string, payload, out any, allowed ...int) bool {
		status, err := f.api(ctx, method, path, payload, out)
		for _, want := range allowed {
			if err == nil && status == want {
				return true
			}
		}
		f.t.Errorf("CLEANUP FAILED %s %s HTTP=%d err=%v; prefix=%s", method, path, status, err, f.prefix)
		return false
	}
	// Query the exact base to recover a PR even if its creation response was lost.
	for page := 1; ; page++ {
		var prs []forgejoLivePR
		path := fmt.Sprintf("/pulls?state=all&base=%s&limit=50&page=%d", url.QueryEscape(f.base), page)
		if !call(http.MethodGet, path, nil, &prs, http.StatusOK) || len(prs) == 0 {
			break
		}
		for _, pr := range prs {
			if pr.Base.Ref != f.base || (pr.Head.Ref != f.clean && pr.Head.Ref != f.conflict) || pr.User.Login != f.actor || pr.Title != "Looper provider contract: "+strings.TrimPrefix(pr.Head.Ref, f.prefix+"-") {
				f.t.Errorf("CLEANUP ownership mismatch for PR=%d; left untouched", pr.Number)
				continue
			}
			path := fmt.Sprintf("/pulls/%d", pr.Number)
			if !pr.Merged && pr.State == "open" {
				if f.merge {
					call(http.MethodDelete, path+"/merge", nil, nil, http.StatusNoContent, http.StatusNotFound)
				}
				call(http.MethodPatch, path, map[string]string{"state": "closed"}, nil, http.StatusCreated, http.StatusOK)
			}
			var final forgejoLivePR
			if call(http.MethodGet, path, nil, &final, http.StatusOK) {
				if final.State != "closed" {
					f.t.Errorf("CLEANUP PR=%d remains %s", pr.Number, final.State)
				} else {
					f.t.Logf("cleanup PR=%d state=%s merged=%v", pr.Number, final.State, final.Merged)
				}
			}
		}
	}
	if f.protection {
		path := "/branch_protections/" + url.PathEscape(f.base)
		call(http.MethodDelete, path, nil, nil, http.StatusNoContent, http.StatusNotFound)
		if call(http.MethodGet, path, nil, nil, http.StatusNotFound) {
			f.t.Logf("cleanup protection=%s absent (HTTP 404)", f.base)
		}
	}
	for _, branch := range []string{f.clean, f.conflict, f.base} {
		path := "/branches/" + url.PathEscape(branch)
		call(http.MethodDelete, path, nil, nil, http.StatusNoContent, http.StatusNotFound)
		if call(http.MethodGet, path, nil, nil, http.StatusNotFound) {
			f.t.Logf("cleanup branch=%s absent (HTTP 404)", branch)
		}
	}
	for branch, before := range f.unchanged {
		var after forgejoLiveBranch
		if call(http.MethodGet, "/branches/"+url.PathEscape(branch), nil, &after, http.StatusOK) {
			if after.Commit.ID != before {
				f.t.Errorf("protected baseline branch %s changed: before=%s after=%s", branch, before, after.Commit.ID)
			} else {
				f.t.Logf("cleanup baseline branch=%s before=%s after=%s unchanged", branch, before, after.Commit.ID)
			}
		}
	}
}

func forgejoLiveValidateSSHURL(raw, repo string) error {
	if strings.HasPrefix(raw, "git@") {
		host, path, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if ok && host != "" && !strings.ContainsAny(host, "/ \t\r\n") && strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".git") == repo {
			return nil
		}
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme == "ssh" && parsed.Hostname() != "" && parsed.User != nil && parsed.User.Username() == "git" && parsed.RawQuery == "" && parsed.Fragment == "" && strings.TrimSuffix(strings.TrimPrefix(parsed.Path, "/"), ".git") == repo {
		if _, hasPassword := parsed.User.Password(); !hasPassword {
			return nil
		}
	}
	return fmt.Errorf("repository ssh_url must be a Git SSH URL for the exact configured repository %s", repo)
}

func TestForgejoLiveFixtureUsesHTTPStatusAndExactSSHRepository(t *testing.T) {
	tea := writeFakeTeaBinary(t, "https://forge.example", "live-test", map[string]fakeTeaAPIRoute{
		"GET /repos/core/sandbox/branches/missing": {Status: "HTTP/2.0 404 Not Found", Body: `{"message":"missing"}`},
	})
	f := forgejoLiveFixture{tea: tea, login: "live-test", repo: "core/sandbox"}
	status, err := f.api(context.Background(), http.MethodGet, "/branches/missing", nil, nil)
	if err != nil || status != http.StatusNotFound {
		t.Fatalf("tea exits zero on an HTTP error: status=%d err=%v", status, err)
	}
	for _, tc := range []struct {
		url   string
		valid bool
	}{
		{"ssh://git@ssh.forge.example/core/sandbox.git", true},
		{"git@ssh.forge.example:core/sandbox.git", true},
		{"ssh://git@ssh.forge.example:2222/core/sandbox.git", true},
		{"ssh://git@ssh.forge.example/core/production.git", false},
		{"git@ssh.forge.example:core/production.git", false},
		{"ssh://git:credential@ssh.forge.example/core/sandbox.git", false},
		{"https://forge.example/core/sandbox.git", false},
		{"ext::unexpected command", false},
	} {
		if err := forgejoLiveValidateSSHURL(tc.url, "core/sandbox"); (err == nil) != tc.valid {
			t.Errorf("SSH repository validation for %q: %v, want valid=%v", tc.url, err, tc.valid)
		}
	}
}
