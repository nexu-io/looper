package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/config"
)

// Queries are daemon-authored; callers supply only a PR number and optionally
// a thread ID whose membership is established by the repository PR query.
const hostThreadsQuery = `query($owner:String!,$repo:String!,$number:Int!,$cursor:String){repository(owner:$owner,name:$repo){pullRequest(number:$number){reviewThreads(first:100,after:$cursor){nodes{id isResolved comments(first:100){nodes{id updatedAt body author{login}} pageInfo{hasNextPage endCursor}}} pageInfo{hasNextPage endCursor}}}}}`
const hostThreadCommentsQuery = `query($id:ID!,$cursor:String!){node(id:$id){... on PullRequestReviewThread{id comments(first:100,after:$cursor){nodes{id updatedAt body author{login}} pageInfo{hasNextPage endCursor}}}}}`

type HostThreadComment struct {
	ID        string `json:"id"`
	UpdatedAt string `json:"updatedAt"`
	Body      string `json:"body"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
}

type HostThread struct {
	ID         string              `json:"id"`
	IsResolved bool                `json:"isResolved"`
	Comments   []HostThreadComment `json:"comments"`
}

type hostPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}
type hostThreadComments struct {
	Nodes    []HostThreadComment `json:"nodes"`
	PageInfo hostPageInfo        `json:"pageInfo"`
}
type hostThreadNode struct {
	ID         string             `json:"id"`
	IsResolved bool               `json:"isResolved"`
	Comments   hostThreadComments `json:"comments"`
}

func (b *hostBroker) readThreads(ctx context.Context, pr int64, target string) ([]byte, error) {
	if b.session.Target().Kind != config.ProviderKindGitHub {
		return nil, errors.New("Forgejo uses native review comment reads; GitHub threads are unsupported")
	}
	repo := strings.Split(b.session.Target().Repo, "/")
	var threads []HostThread
	var cursor *string
	total := 0
	requests := 0
	seenThreads := map[string]bool{}
	for page := 0; page < maxHostPages; page++ {
		var response struct {
			Data struct {
				Repository *struct {
					PullRequest *struct {
						ReviewThreads struct {
							Nodes    []hostThreadNode `json:"nodes"`
							PageInfo hostPageInfo     `json:"pageInfo"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := b.threadQuery(ctx, hostThreadsQuery, map[string]any{"owner": repo[0], "repo": repo[1], "number": pr, "cursor": cursor}, &response, &total, &requests); err != nil {
			return nil, err
		}
		if response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
			return nil, errors.New("hosting thread repository or pull request was not found")
		}
		collection := response.Data.Repository.PullRequest.ReviewThreads
		for _, node := range collection.Nodes {
			if node.ID == "" || seenThreads[node.ID] {
				return nil, errors.New("hosting thread pagination returned duplicate or invalid IDs")
			}
			seenThreads[node.ID] = true
			if target != "" && node.ID != target {
				continue
			}
			comments := node.Comments.Nodes
			seenCursors := map[string]bool{}
			for info := node.Comments.PageInfo; info.HasNextPage; {
				if info.EndCursor == "" || seenCursors[info.EndCursor] {
					return nil, errors.New("hosting thread comments contain invalid pagination")
				}
				seenCursors[info.EndCursor] = true
				var more struct {
					Data struct {
						Node *hostThreadNode `json:"node"`
					} `json:"data"`
				}
				if err := b.threadQuery(ctx, hostThreadCommentsQuery, map[string]any{"id": node.ID, "cursor": info.EndCursor}, &more, &total, &requests); err != nil {
					return nil, err
				}
				if more.Data.Node == nil || more.Data.Node.ID != node.ID {
					return nil, errors.New("hosting thread comments changed identity")
				}
				comments = append(comments, more.Data.Node.Comments.Nodes...)
				info = more.Data.Node.Comments.PageInfo
			}
			if comments == nil {
				comments = []HostThreadComment{}
			}
			seenComments := map[string]bool{}
			for _, comment := range comments {
				if comment.ID == "" || comment.UpdatedAt == "" || seenComments[comment.ID] {
					return nil, errors.New("hosting thread comments have missing or duplicate observation identifiers")
				}
				seenComments[comment.ID] = true
			}
			thread := HostThread{ID: node.ID, IsResolved: node.IsResolved, Comments: comments}
			if target != "" {
				return json.Marshal(thread)
			}
			threads = append(threads, thread)
		}
		if !collection.PageInfo.HasNextPage {
			if target != "" {
				return nil, errors.New("thread is not part of the bound repository pull request")
			}
			if threads == nil {
				threads = []HostThread{}
			}
			return json.Marshal(threads)
		}
		if collection.PageInfo.EndCursor == "" || cursor != nil && *cursor == collection.PageInfo.EndCursor {
			return nil, errors.New("hosting threads contain invalid pagination")
		}
		next := collection.PageInfo.EndCursor
		cursor = &next
	}
	return nil, errors.New("hosting thread pagination exceeds page limit")
}

func (b *hostBroker) threadQuery(ctx context.Context, query string, variables map[string]any, result any, total, requests *int) error {
	*requests++
	if *requests > maxHostPages {
		return errors.New("hosting thread pagination exceeds request limit")
	}
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	data, _, err := b.request(ctx, http.MethodPost, "/graphql", payload, "application/json")
	if err != nil {
		return err
	}
	*total += len(data)
	if *total > maxHostResponseBytes {
		return errors.New("hosting thread response exceeds size limit")
	}
	var envelope struct {
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return errors.New("hosting GraphQL response is invalid")
	}
	if len(envelope.Errors) > 0 {
		return errors.New("hosting server rejected the thread query")
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("hosting thread response is invalid")
	}
	return nil
}
