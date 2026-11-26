package collab

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/forge"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
)

// Resolve the provider from the PR URL, never from a task's working directory.
// One task can follow PRs across repositories and hosting platforms.
type pullRequestObserver struct {
	cfg            *config.Config
	github         *githubinfra.Gateway
	clients        map[string]*forge.ForgejoClient
	teaRunner      forge.TeaCommandRunner
	forgejoOptions []forge.ForgejoOption
}

func (o *pullRequestObserver) view(ctx context.Context, link string) (githubinfra.PullRequestDetail, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return githubinfra.PullRequestDetail{}, fmt.Errorf("invalid HTTPS PR URL: %s", link)
	}
	if strings.EqualFold(u.Host, "github.com") {
		repo, number, err := parsePullRequestURL(link)
		if err != nil {
			return githubinfra.PullRequestDetail{}, err
		}
		if o.github == nil {
			return githubinfra.PullRequestDetail{}, fmt.Errorf("GitHub client unavailable")
		}
		pr, err := o.github.ViewPullRequestForReviewer(ctx, githubinfra.ViewPullRequestInput{Repo: repo, PRNumber: number})
		if err != nil {
			return githubinfra.PullRequestDetail{}, err
		}
		threads, err := o.github.ListReviewThreads(ctx, githubinfra.ListReviewThreadsInput{Repo: repo, PRNumber: number, AllPages: true})
		if err != nil {
			return githubinfra.PullRequestDetail{}, err
		}
		// Keep full reply evidence, and fingerprint individual external comments
		// rather than a thread fingerprint that also changes on our own replies.
		pr.Comments = nil
		for _, thread := range threads {
			for _, comment := range thread.Comments {
				pr.Comments = append(pr.Comments, map[string]any{"id": comment.ID, "threadId": thread.ID, "isResolved": thread.IsResolved, "body": comment.Body, "author": comment.Author, "url": comment.URL, "path": thread.Path, "commit": map[string]any{"oid": comment.CommitOID}})
			}
		}
		return pr, nil
	}
	provider, err := o.forgejoProvider(ctx, u)
	if err != nil {
		return githubinfra.PullRequestDetail{}, err
	}
	base, _ := url.Parse(provider.BaseURL)
	path := strings.TrimPrefix(u.Path, strings.TrimRight(base.Path, "/")+"/")
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pulls" || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return githubinfra.PullRequestDetail{}, fmt.Errorf("not a Forgejo PR URL: %s", link)
	}
	number, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || number <= 0 {
		return githubinfra.PullRequestDetail{}, fmt.Errorf("invalid PR number: %s", link)
	}
	repo := parts[0] + "/" + parts[1]
	key := provider.BaseURL + "/" + repo
	client := o.clients[key]
	if client == nil {
		options := append([]forge.ForgejoOption{}, o.forgejoOptions...)
		if o.teaRunner != nil {
			options = append(options, forge.WithTeaRunner(o.teaRunner))
		}
		client, err = forge.NewForgejoClientFromConfig(provider, repo, options...)
		if err != nil {
			return githubinfra.PullRequestDetail{}, err
		}
		if o.clients == nil {
			o.clients = map[string]*forge.ForgejoClient{}
		}
		o.clients[key] = client
	}
	return forgejoPullRequestSnapshot(ctx, client, number)
}

func (o *pullRequestObserver) forgejoProvider(ctx context.Context, target *url.URL) (config.ProviderConfig, error) {
	matches := func(raw string) bool {
		base, err := url.Parse(raw)
		return err == nil && base.Scheme == target.Scheme && strings.EqualFold(base.Host, target.Host) && strings.HasPrefix(target.Path, strings.TrimRight(base.Path, "/")+"/")
	}
	var candidates []config.ProviderConfig
	if o.cfg != nil {
		for _, provider := range o.cfg.Providers {
			if provider.Kind == config.ProviderKindForgejo && matches(provider.BaseURL) {
				candidates = append(candidates, provider)
			}
		}
	}
	if len(candidates) == 0 {
		teaPath, err := forge.ResolveTeaPath(config.ProviderConfig{}, nil)
		if err != nil {
			return config.ProviderConfig{}, err
		}
		logins, err := forge.ListTeaLogins(ctx, teaPath, o.teaRunner)
		if err != nil {
			return config.ProviderConfig{}, err
		}
		for _, login := range logins {
			if matches(login.URL) {
				name := login.Name
				candidates = append(candidates, config.ProviderConfig{ID: "tea:" + name, Kind: config.ProviderKindForgejo, BaseURL: login.URL, Auth: config.ProviderAuthTea, TeaPath: &teaPath, TeaLogin: &name})
			}
		}
	}
	if len(candidates) != 1 {
		return config.ProviderConfig{}, fmt.Errorf("Forgejo %s: found %d matching providers/tea logins; configure one explicit providers[] entry for this host", target.Host, len(candidates))
	}
	return candidates[0], nil
}

func forgejoPullRequestSnapshot(ctx context.Context, client *forge.ForgejoClient, number int64) (githubinfra.PullRequestDetail, error) {
	pr, err := client.ViewPullRequest(ctx, number)
	if err != nil {
		return githubinfra.PullRequestDetail{}, err
	}
	result := githubinfra.PullRequestDetail{Number: pr.Number, Title: pr.Title, Body: pr.Body, URL: pr.HTMLURL, State: strings.ToUpper(pr.State), HeadSHA: pr.Head.SHA, BaseSHA: pr.Base.SHA, HeadRefName: pr.Head.Name, BaseRefName: pr.Base.Name, Author: pr.User.Login, IsDraft: pr.IsDraft, Mergeable: pr.Mergeable, MergedAt: pr.MergedAt}
	if pr.Merged {
		result.State = "MERGED"
	}
	for _, label := range pr.Labels {
		result.Labels = append(result.Labels, label.Name)
	}
	for _, reviewer := range pr.Reviewers {
		result.ReviewRequests = append(result.ReviewRequests, reviewer.Login)
	}
	if result.State == "CLOSED" || result.State == "MERGED" {
		return result, nil
	}
	// mergeable=false is NOT a confirmed conflict (it also means checking/WIP).
	// Retain the tri-state: changes wake the Agent to inspect the exact commits
	// in its own repository, without inventing a host-side repository binding.
	reviews, err := client.ListPullRequestReviews(ctx, number)
	if err != nil {
		return githubinfra.PullRequestDetail{}, err
	}
	for _, review := range reviews {
		comments := make([]map[string]any, 0, len(review.Comments))
		for _, comment := range review.Comments {
			comments = append(comments, map[string]any{"id": comment.ID, "body": comment.Body, "url": comment.HTMLURL, "author": map[string]any{"login": comment.User.Login}, "path": comment.Path, "diffHunk": comment.DiffHunk, "commit": map[string]any{"oid": comment.CommitID}, "resolver": comment.Resolver})
		}
		result.Reviews = append(result.Reviews, map[string]any{"id": review.ID, "state": review.State, "body": review.Body, "url": review.HTMLURL, "author": map[string]any{"login": review.User.Login}, "commit": map[string]any{"oid": review.CommitID}, "dismissed": review.Dismissed, "stale": review.Stale, "comments": comments})
	}
	comments, err := client.ListIssueComments(ctx, number)
	if err != nil {
		return githubinfra.PullRequestDetail{}, err
	}
	for _, comment := range comments {
		result.IssueComments = append(result.IssueComments, githubinfra.CommentInfo{ID: comment.ID, Body: comment.Body, URL: comment.HTMLURL, Author: comment.User.Login, UpdatedAt: comment.UpdatedAt})
	}
	checks, err := client.ListCommitChecks(ctx, pr.Head.SHA)
	if err != nil {
		return githubinfra.PullRequestDetail{}, err
	}
	for _, check := range checks {
		result.Checks = append(result.Checks, map[string]any{"id": check.ID, "name": check.Name, "state": check.State, "url": check.URL, "description": check.Description, "actionRunID": check.ActionRunID})
	}
	return result, nil
}

// Wake on green CI/preview, approvals, base changes and closure as well as
// failures. Ignore broad UpdatedAt and the PR author's own progress comments:
// responding to a review must not create an endless self-triggering loop.
func pullRequestSignal(pr githubinfra.PullRequestDetail) string {
	signal := pr
	signal.Body = ""
	signal.CreatedAt = ""
	signal.UpdatedAt = ""
	signal.ClosedAt = ""
	signal.CommentCount = 0
	signal.Checks = nil
	for _, check := range pr.Checks {
		semantic := map[string]any{}
		for key, value := range check {
			// Re-publishing the same external status allocates a new ID.
			// A new Actions run, changed state/log URL or description still wakes.
			if key != "id" {
				semantic[key] = value
			}
		}
		signal.Checks = append(signal.Checks, semantic)
	}
	signal.Comments = externalPRComments(pr.Comments, pr.Author)
	signal.IssueComments = nil
	for _, comment := range pr.IssueComments {
		if !strings.EqualFold(comment.Author, pr.Author) {
			signal.IssueComments = append(signal.IssueComments, comment)
		}
	}
	signal.Reviews = nil
	for _, review := range pr.Reviews {
		user, _ := review["author"].(map[string]any)
		login, _ := user["login"].(string)
		if pr.Author != "" && strings.EqualFold(login, pr.Author) && strings.EqualFold(fmt.Sprint(review["state"]), "COMMENTED") {
			continue
		}
		copy := map[string]any{}
		for key, value := range review {
			copy[key] = value
		}
		if comments, ok := review["comments"].([]map[string]any); ok {
			copy["comments"] = externalPRComments(comments, pr.Author)
		}
		signal.Reviews = append(signal.Reviews, copy)
	}
	raw, _ := json.Marshal(signal)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func externalPRComments(comments []map[string]any, author string) []map[string]any {
	var result []map[string]any
	for _, comment := range comments {
		login, _ := comment["author"].(string)
		if user, ok := comment["author"].(map[string]any); ok {
			login, _ = user["login"].(string)
		}
		if author == "" || !strings.EqualFold(login, author) {
			result = append(result, comment)
		}
	}
	return result
}
