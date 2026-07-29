// Package planedoc reads and writes Plane spec documents (Pages) and associates
// them with work items via native Plane work-item Links, by shelling out to the
// `plane` CLI — the same way the github package shells out to `gh`. This is how
// looper's tech-spec pipeline (plan §8.2/§8.4) stores specs in Plane instead of as
// repo files, and how it answers "which page is this work item's product spec"
// (the page↔work-item convention: a native link tagged looper:product-spec /
// looper:tech-spec, spike-verified 2026-07-07).
package planedoc

import (
	"context"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"regexp"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
)

const defaultPlaneCommandTimeout = 45 * time.Second

// Machine-parseable link titles marking which Plane page is a work item's spec.
// The reverse lookup filters a work item's links by these exact titles.
const (
	ProductSpecLinkTitle = "looper:product-spec"
	TechSpecLinkTitle    = "looper:tech-spec"
)

// RunFunc runs a `plane` invocation; injectable so tests fake the CLI.
type RunFunc func(context.Context, shell.Options) (shell.Result, error)

type Options struct {
	// PlanePath is the `plane` binary; defaults to "plane" (resolved on PATH).
	PlanePath string
	// APIBaseURL / APIKey / Workspace are passed explicitly to every call so looper
	// drives Plane deterministically with the teammate's own key, not plane.toml.
	APIBaseURL string
	APIKey     string
	Workspace  string
	Run        RunFunc
}

type Gateway struct {
	planePath  string
	apiBaseURL string
	apiKey     string
	workspace  string
	run        RunFunc
}

func New(o Options) *Gateway {
	planePath := strings.TrimSpace(o.PlanePath)
	if planePath == "" {
		planePath = "plane"
	}
	run := o.Run
	if run == nil {
		run = shell.Run
	}
	return &Gateway{
		planePath:  planePath,
		apiBaseURL: strings.TrimSpace(o.APIBaseURL),
		apiKey:     strings.TrimSpace(o.APIKey),
		workspace:  strings.TrimSpace(o.Workspace),
		run:        run,
	}
}

// Page is a Plane spec document.
type Page struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentHTML string `json:"description_html"`
	CreatedBy   string `json:"created_by"`
	UpdatedBy   string `json:"updated_by"`
	OwnedBy     string `json:"owned_by"`
	// URL is the human-clickable page URL (constructed; Plane's page API returns no
	// URL). The page id is embedded, so a link back to it is reverse-parseable.
	URL string
}

// Link is a native Plane work-item link.
type Link struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// globalArgs are the auth/coordinate flags every call carries. Only non-empty ones
// are passed, so an unset field falls through to the CLI's plane.toml / env.
func (g *Gateway) globalArgs() []string {
	args := make([]string, 0, 6)
	if g.apiBaseURL != "" {
		args = append(args, "--api-base-url", g.apiBaseURL)
	}
	if g.apiKey != "" {
		args = append(args, "--api-key", g.apiKey)
	}
	if g.workspace != "" {
		args = append(args, "--workspace", g.workspace)
	}
	return args
}

func (g *Gateway) runPlane(ctx context.Context, stdin string, args ...string) (shell.Result, error) {
	return g.run(ctx, shell.Options{
		Command: g.planePath,
		Args:    args,
		Stdin:   stdin,
		Timeout: defaultPlaneCommandTimeout,
	})
}

// CreatePage creates a Plane page whose body is `bodyMarkdown` (converted to HTML
// server-side) and returns it with a constructed web URL.
func (g *Gateway) CreatePage(ctx context.Context, projectID, name, bodyMarkdown string) (Page, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(name) == "" {
		return Page{}, fmt.Errorf("planedoc: CreatePage requires project id and name")
	}
	// Attached form (--body=VALUE) so a body that begins with YAML front matter
	// ("---") is taken as the flag value, not parsed as a stray flag/positional by
	// the CLI's clap parser (which otherwise rejects it: "unexpected argument '---'").
	args := []string{"api", "page", "create", "--project", projectID, "--name", name, "--body=" + bodyMarkdown, "--format", "md", "--json"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return Page{}, fmt.Errorf("planedoc: create page: %w", err)
	}
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &payload); err != nil {
		return Page{}, fmt.Errorf("planedoc: decode created page: %w", err)
	}
	if strings.TrimSpace(payload.ID) == "" {
		return Page{}, fmt.Errorf("planedoc: created page has no id")
	}
	return Page{ID: payload.ID, Name: payload.Name, URL: g.pageWebURL(projectID, payload.ID)}, nil
}

// PageContent returns a page's body as HTML.
func (g *Gateway) PageContent(ctx context.Context, projectID, pageID string) (string, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(pageID) == "" {
		return "", fmt.Errorf("planedoc: PageContent requires project id and page id")
	}
	args := []string{"api", "page", "get", "--project", projectID, pageID, "--content"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return "", fmt.Errorf("planedoc: get page content: %w", err)
	}
	return strings.TrimRight(result.Stdout, "\n"), nil
}

// PageDocument returns a page's content and authorship metadata. Product-spec
// callers use this instead of PageContent because a non-empty page is not enough:
// the configured product owner must have created, owned, or last updated it.
func (g *Gateway) PageDocument(ctx context.Context, projectID, pageID string) (Page, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(pageID) == "" {
		return Page{}, fmt.Errorf("planedoc: PageDocument requires project id and page id")
	}
	args := []string{"api", "page", "get", "--project", projectID, pageID, "--json"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return Page{}, fmt.Errorf("planedoc: get page document: %w", err)
	}
	var page Page
	if err := json.Unmarshal([]byte(result.Stdout), &page); err != nil {
		return Page{}, fmt.Errorf("planedoc: decode page document: %w", err)
	}
	if strings.TrimSpace(page.ID) == "" {
		return Page{}, fmt.Errorf("planedoc: page document has no id")
	}
	page.ContentHTML = strings.TrimSpace(page.ContentHTML)
	page.URL = g.pageWebURL(projectID, page.ID)
	return page, nil
}

// AuthoredBy reports whether Plane attributes the page to memberID. Any one of
// creator, owner, or latest editor is enough: product may take ownership of an
// existing shell and complete it, but a page touched only by Looper's account is
// never accepted as product-authored.
func (p Page) AuthoredBy(memberID string) bool {
	memberID = strings.TrimSpace(memberID)
	if memberID == "" {
		return false
	}
	return strings.TrimSpace(p.CreatedBy) == memberID ||
		strings.TrimSpace(p.UpdatedBy) == memberID ||
		strings.TrimSpace(p.OwnedBy) == memberID
}

// UpdatePageContent replaces a Plane page's body with bodyMarkdown (converted to HTML
// server-side) — how looper re-publishes a spec the grill agent revised in the worktree
// (the agent can't write to Plane directly under the codex sandbox).
func (g *Gateway) UpdatePageContent(ctx context.Context, projectID, pageID, bodyMarkdown string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(pageID) == "" {
		return fmt.Errorf("planedoc: UpdatePageContent requires project id and page id")
	}
	// Attached form (--body=VALUE) — see CreatePage: a body starting with "---"
	// front matter must not be misparsed as a flag by the CLI.
	args := []string{"api", "page", "update", "--project", projectID, pageID, "--body=" + bodyMarkdown, "--format", "md"}
	args = append(args, g.globalArgs()...)
	if _, err := g.runPlane(ctx, "", args...); err != nil {
		return fmt.Errorf("planedoc: update page content: %w", err)
	}
	return nil
}

// ListWorkItemLinks returns a work item's native links.
func (g *Gateway) ListWorkItemLinks(ctx context.Context, projectID, workItemID string) ([]Link, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" {
		return nil, fmt.Errorf("planedoc: ListWorkItemLinks requires project id and work item id")
	}
	args := []string{"api", "link", "list", "--project", projectID, "--work-item", workItemID, "--all", "--json"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("planedoc: list links: %w", err)
	}
	return decodeLinks(result.Stdout)
}

// FindSpecLink returns the URL of the work item's link tagged with `title`
// (e.g. ProductSpecLinkTitle), and whether one exists. This is the reverse lookup
// "which page is this work item's spec".
func (g *Gateway) FindSpecLink(ctx context.Context, projectID, workItemID, title string) (string, bool, error) {
	links, err := g.ListWorkItemLinks(ctx, projectID, workItemID)
	if err != nil {
		return "", false, err
	}
	for _, link := range links {
		if strings.EqualFold(strings.TrimSpace(link.Title), strings.TrimSpace(title)) {
			return link.URL, true, nil
		}
	}
	return "", false, nil
}

// UpsertSpecLink attaches (or re-points) the work item's spec link for `title` to
// `url`. Idempotent: a matching link with the same URL is left alone; a stale one
// is updated in place; otherwise a new link is created. This is how looper
// associates a spec page with its work item — including on the human's behalf when
// they just drop a spec in the thread (§8.3).
func (g *Gateway) UpsertSpecLink(ctx context.Context, projectID, workItemID, title, url string) error {
	if strings.TrimSpace(url) == "" || strings.TrimSpace(title) == "" {
		return fmt.Errorf("planedoc: UpsertSpecLink requires title and url")
	}
	links, err := g.ListWorkItemLinks(ctx, projectID, workItemID)
	if err != nil {
		return err
	}
	data := fmt.Sprintf(`{"url":%s,"title":%s}`, jsonString(url), jsonString(title))
	for _, link := range links {
		if !strings.EqualFold(strings.TrimSpace(link.Title), strings.TrimSpace(title)) {
			continue
		}
		if strings.TrimSpace(link.URL) == strings.TrimSpace(url) {
			return nil // already associated
		}
		args := []string{"api", "link", "update", "--project", projectID, "--work-item", workItemID, link.ID, "--data", data}
		args = append(args, g.globalArgs()...)
		if _, err := g.runPlane(ctx, "", args...); err != nil {
			return fmt.Errorf("planedoc: update spec link: %w", err)
		}
		return nil
	}
	args := []string{"api", "link", "create", "--project", projectID, "--work-item", workItemID, "--data", data, "--json"}
	args = append(args, g.globalArgs()...)
	if _, err := g.runPlane(ctx, "", args...); err != nil {
		return fmt.Errorf("planedoc: create spec link: %w", err)
	}
	return nil
}

// ReadSpec returns the content of the work item's spec page tagged with `title`
// (ProductSpecLinkTitle / TechSpecLinkTitle), resolving link → page → body. The
// second return is whether such a spec is associated at all. This is how the worker
// reads the product / tech spec from Plane instead of a repo file (§8.4).
func (g *Gateway) ReadSpec(ctx context.Context, projectID, workItemID, title string) (string, bool, error) {
	url, found, err := g.FindSpecLink(ctx, projectID, workItemID, title)
	if err != nil || !found {
		return "", false, err
	}
	pageID := PageIDFromURL(url)
	if pageID == "" {
		// The link points somewhere that isn't a Plane page we can read (e.g. a
		// Feishu doc a human dropped). Surface the URL so the caller can still use it.
		return url, true, nil
	}
	content, err := g.PageContent(ctx, projectID, pageID)
	if err != nil {
		return "", true, err
	}
	return content, true, nil
}

// ReadPlanePageSpec is the strict authority reader for formal product gates. A
// matching link must point to a native Plane page and that page must have non-empty
// content; external URLs and blank placeholder pages do not satisfy the gate.
func (g *Gateway) ReadPlanePageSpec(ctx context.Context, projectID, workItemID, title string) (string, bool, error) {
	pageURL, found, err := g.FindSpecLink(ctx, projectID, workItemID, title)
	if err != nil || !found {
		return "", false, err
	}
	pageID := PageIDFromURL(pageURL)
	if pageID == "" {
		return "", false, nil
	}
	content, err := g.PageContent(ctx, projectID, pageID)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(content) == "" {
		return "", false, nil
	}
	return content, true, nil
}

// WriteTechSpec creates a Plane page for the tech spec and associates it with the
// work item as its looper:tech-spec link (idempotent). Returns the page. This is
// looper's "write the tech spec + link it" step (§8.4).
func (g *Gateway) WriteTechSpec(ctx context.Context, projectID, workItemID, name, bodyMarkdown string) (Page, error) {
	page, err := g.CreatePage(ctx, projectID, name, bodyMarkdown)
	if err != nil {
		return Page{}, err
	}
	if err := g.UpsertSpecLink(ctx, projectID, workItemID, TechSpecLinkTitle, page.URL); err != nil {
		return page, err
	}
	return page, nil
}

// PageIDFromURL extracts a Plane page id from a page URL of the form
// .../pages/<uuid>[/]. Returns "" when the URL isn't a Plane page link.
func PageIDFromURL(pageURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(strings.SplitN(pageURL, "#", 2)[0]), "/")
	marker := "/pages/"
	i := strings.LastIndex(trimmed, marker)
	if i < 0 {
		return ""
	}
	id := trimmed[i+len(marker):]
	if strings.ContainsAny(id, "/?#") {
		return ""
	}
	return id
}

// PageCommentURL returns the exact browser destination for a decision comment.
// Plane currently exposes page comments as anchors on the page, so notification
// transports can remain one-way and still take the owner directly to the source.
func PageCommentURL(pageURL, commentID string) string {
	pageURL = strings.TrimRight(strings.TrimSpace(strings.SplitN(pageURL, "#", 2)[0]), "/")
	commentID = strings.TrimSpace(commentID)
	if pageURL == "" || commentID == "" {
		return ""
	}
	return pageURL + "#comment-" + commentID
}

func WorkItemCommentURL(workItemURL, commentID string) string {
	return PageCommentURL(workItemURL, commentID)
}

// IntakeAction is what a looper:auto feature work item needs next at the intake
// gate (plan §8.0 / flowchart node D): proceed to the tech-spec pipeline, or ask
// the product owner to supply a product spec first and wait.
type IntakeAction string

const (
	IntakeProceed        IntakeAction = "proceed"         // product spec present → planner can write the tech spec
	IntakeRequestProduct IntakeAction = "request_product" // no product spec → @product + wait
)

// DecideIntake maps "does this feature have a product spec?" to the gate action.
func DecideIntake(hasProductSpec bool) IntakeAction {
	if hasProductSpec {
		return IntakeProceed
	}
	return IntakeRequestProduct
}

// HasProductSpec reports whether the work item already has a product spec linked
// (flowchart node D). Returns the spec URL too.
func (g *Gateway) HasProductSpec(ctx context.Context, projectID, workItemID string) (bool, string, error) {
	url, found, err := g.FindSpecLink(ctx, projectID, workItemID, ProductSpecLinkTitle)
	if err != nil || !found || PageIDFromURL(url) == "" {
		return false, url, err
	}
	content, readErr := g.PageContent(ctx, projectID, PageIDFromURL(url))
	if readErr != nil {
		return false, url, readErr
	}
	return strings.TrimSpace(content) != "", url, nil
}

// WorkItemComment preserves Plane's UUID identity and timestamps. Decision routing
// must use Actor as its authority; display names are intentionally not part of the
// contract returned by Plane's work-item comment API.
type WorkItemComment struct {
	ID              string `json:"id"`
	Actor           string `json:"actor"`
	CommentHTML     string `json:"comment_html"`
	CommentStripped string `json:"comment_stripped"`
	Description     string `json:"description"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func (g *Gateway) workItemCommentsPath(projectID, workItemID string) (string, error) {
	ws := strings.TrimSpace(g.workspace)
	if ws == "" || strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" {
		return "", fmt.Errorf("planedoc: work item comments require workspace, project id, and work item id")
	}
	return fmt.Sprintf("workspaces/%s/projects/%s/work-items/%s/comments/", ws, projectID, workItemID), nil
}

// CreateWorkItemComment posts an HTML comment and returns Plane's stable receipt.
func (g *Gateway) CreateWorkItemComment(ctx context.Context, projectID, workItemID, commentHTML string) (WorkItemComment, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" || strings.TrimSpace(commentHTML) == "" {
		return WorkItemComment{}, fmt.Errorf("planedoc: CreateWorkItemComment requires project id, work item id, and html")
	}
	path, err := g.workItemCommentsPath(projectID, workItemID)
	if err != nil {
		return WorkItemComment{}, err
	}
	data := fmt.Sprintf(`{"comment_html":%s}`, jsonString(commentHTML))
	args := []string{"api", "request", path, "--method", "POST", "--data", data}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return WorkItemComment{}, fmt.Errorf("planedoc: comment on work item: %w", err)
	}
	var comment WorkItemComment
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &comment); err != nil {
		return WorkItemComment{}, fmt.Errorf("planedoc: decode work item comment: %w", err)
	}
	return comment, nil
}

// ListWorkItemComments returns all comments while preserving actor/comment UUIDs.
func (g *Gateway) ListWorkItemComments(ctx context.Context, projectID, workItemID string) ([]WorkItemComment, error) {
	path, err := g.workItemCommentsPath(projectID, workItemID)
	if err != nil {
		return nil, err
	}
	args := []string{"api", "request", path, "--method", "GET"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("planedoc: list work item comments: %w", err)
	}
	var envelope struct {
		Results []WorkItemComment `json:"results"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &envelope); err == nil && envelope.Results != nil {
		return envelope.Results, nil
	}
	var comments []WorkItemComment
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &comments); err != nil {
		return nil, fmt.Errorf("planedoc: decode work item comments: %w", err)
	}
	return comments, nil
}

func (g *Gateway) CommentOnWorkItem(ctx context.Context, projectID, workItemID, commentHTML string) error {
	_, err := g.CreateWorkItemComment(ctx, projectID, workItemID, commentHTML)
	return err
}

// RequestProductSpec asks the product owner (by an already-rendered mention/name)
// to supply a product spec, as a comment on the work item (flowchart node E, Plane
// side — the task-card @-mention in Feishu is a separate surface). The comment
// tells them looper will auto-associate whatever spec link/text they reply with.
func (g *Gateway) RequestProductSpec(ctx context.Context, projectID, workItemID, ownerMention, workItemName string) (WorkItemComment, error) {
	html := fmt.Sprintf(
		"<p>%s 请先为需求「%s」补一份可执行的 product spec，再让 looper 开始技术梳理。</p><p>至少写清：用户问题与目标、首版范围和非目标、关键交互或输出、验收标准；涉及付费策略或阶段优先级，也请直接在 spec 中定下来。</p><p>请由产品负责人创建或更新方案页，或由产品负责人在这条评论下明确回复方案链接/正文。Looper 不会代写产品范围；验证产品身份后才会关联到本 work item 并继续。</p>",
		htmlpkg.EscapeString(strings.TrimSpace(ownerMention)),
		htmlpkg.EscapeString(strings.TrimSpace(workItemName)),
	)
	comments, err := g.ListWorkItemComments(ctx, projectID, workItemID)
	if err != nil {
		return WorkItemComment{}, err
	}
	for index := len(comments) - 1; index >= 0; index-- {
		if strings.TrimSpace(comments[index].CommentHTML) == html {
			return comments[index], nil
		}
	}
	return g.CreateWorkItemComment(ctx, projectID, workItemID, html)
}

var (
	workItemCommentHrefPattern = regexp.MustCompile(`(?i)\bhref\s*=\s*["']([^"']+)["']`)
	workItemCommentURLPattern  = regexp.MustCompile(`https?://[^\s<>"']+`)
	workItemCommentBreaks      = regexp.MustCompile(`(?i)<br\s*/?>|</(?:p|div|li|h[1-6])\s*>`)
	workItemCommentTags        = regexp.MustCompile(`<[^>]+>`)
)

// DroppedSpecContent maps a reply on the exact Plane ask to the existing
// AssociateDroppedSpec contract. A linked document stays a link; inline text is
// captured into a Plane page before the work-item association is written.
func DroppedSpecContent(comment WorkItemComment) (url, inlineText string) {
	if match := workItemCommentHrefPattern.FindStringSubmatch(comment.CommentHTML); len(match) == 2 {
		return strings.TrimSpace(htmlpkg.UnescapeString(match[1])), ""
	}
	text := strings.TrimSpace(comment.CommentStripped)
	if text == "" {
		text = workItemCommentBreaks.ReplaceAllString(comment.CommentHTML, "\n")
		text = workItemCommentTags.ReplaceAllString(text, "")
		text = strings.TrimSpace(htmlpkg.UnescapeString(text))
	}
	if match := workItemCommentURLPattern.FindString(text); match != "" {
		return strings.TrimSpace(match), ""
	}
	return "", text
}

// PageComment is a Notion-style comment on a Plane page (powerformer/plane PR #11,
// exposed on the public /api/v1). node H uses it as the tech-spec review surface: the
// reviewer posts its assist-review as a page comment, and a human's approve reply is
// polled here. Reached through the `plane api request` escape hatch until the CLI
// grows a typed `page comment` subcommand.
type PageComment struct {
	ID              string `json:"id"`
	CommentHTML     string `json:"comment_html"`
	CommentStripped string `json:"comment_stripped"`
	DisplayName     string `json:"display_name"`
	Actor           string `json:"actor"`
	ExternalID      string `json:"external_id"`
	CreatedAt       string `json:"created_at"`
	ResolvedAt      string `json:"resolved_at"`
}

// pageCommentsPath builds the /api/v1-relative page-comments collection path. The
// workspace slug must be embedded literally (the request escape hatch takes a raw
// path), so it's required here even though globalArgs also passes --workspace.
func (g *Gateway) pageCommentsPath(projectID, pageID string) (string, error) {
	ws := strings.TrimSpace(g.workspace)
	if ws == "" || strings.TrimSpace(projectID) == "" || strings.TrimSpace(pageID) == "" {
		return "", fmt.Errorf("planedoc: page comments require workspace, project id, and page id")
	}
	return fmt.Sprintf("workspaces/%s/projects/%s/pages/%s/comments/", ws, projectID, pageID), nil
}

// CreatePageComment posts an HTML comment on a Plane page — the reviewer's node H
// assist-review, or an automated status note. Returns the created comment.
func (g *Gateway) CreatePageComment(ctx context.Context, projectID, pageID, commentHTML string) (PageComment, error) {
	if strings.TrimSpace(commentHTML) == "" {
		return PageComment{}, fmt.Errorf("planedoc: CreatePageComment requires html")
	}
	path, err := g.pageCommentsPath(projectID, pageID)
	if err != nil {
		return PageComment{}, err
	}
	data := fmt.Sprintf(`{"comment_html":%s}`, jsonString(commentHTML))
	args := []string{"api", "request", path, "--method", "POST", "--data", data}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return PageComment{}, fmt.Errorf("planedoc: create page comment: %w", err)
	}
	var pc PageComment
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &pc); err != nil {
		return PageComment{}, fmt.Errorf("planedoc: decode page comment: %w", err)
	}
	return pc, nil
}

// ListPageComments returns a page's comments, newest-first as Plane orders them. node
// H polls this for a human's approve reply (the gate before dispatching the worker).
func (g *Gateway) ListPageComments(ctx context.Context, projectID, pageID string) ([]PageComment, error) {
	path, err := g.pageCommentsPath(projectID, pageID)
	if err != nil {
		return nil, err
	}
	args := []string{"api", "request", path, "--method", "GET"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("planedoc: list page comments: %w", err)
	}
	return decodePageComments(result.Stdout)
}

// PostSpecReviewComment opens node H on a spec page: it posts a [looper]-marked
// review comment, idempotently — if the page already carries a [looper] comment it
// no-ops, so a planner rerun doesn't duplicate the review request. commentHTML must
// carry LooperCommentMarker. Returns whether it posted.
func (g *Gateway) PostSpecReviewComment(ctx context.Context, projectID, pageURL, commentHTML string) (bool, error) {
	pageID := PageIDFromURL(pageURL)
	if pageID == "" {
		return false, fmt.Errorf("planedoc: PostSpecReviewComment cannot resolve page id from %q", pageURL)
	}
	existing, err := g.ListPageComments(ctx, projectID, pageID)
	if err != nil {
		return false, err
	}
	for _, c := range existing {
		if commentIsLoopers(c) {
			return false, nil // review already opened (looper already commented)
		}
	}
	if _, err := g.CreatePageComment(ctx, projectID, pageID, commentHTML); err != nil {
		return false, err
	}
	return true, nil
}

// CreateCommentOnPageURL posts an HTML comment on the page at pageURL and returns
// the created comment. Approval gates use the returned id/timestamp as the exact
// revision boundary; callers that only need a best-effort audit note can use
// CommentOnPageURL below.
func (g *Gateway) CreateCommentOnPageURL(ctx context.Context, projectID, pageURL, commentHTML string) (PageComment, error) {
	pageID := PageIDFromURL(pageURL)
	if pageID == "" {
		return PageComment{}, fmt.Errorf("planedoc: CreateCommentOnPageURL cannot resolve page id from %q", pageURL)
	}
	return g.CreatePageComment(ctx, projectID, pageID, commentHTML)
}

// CommentOnPageURL posts an HTML comment on the page at pageURL (resolving its id) —
// node H audit notes such as "✅ approved by X". Always posts (no idempotency check),
// unlike PostSpecReviewComment.
func (g *Gateway) CommentOnPageURL(ctx context.Context, projectID, pageURL, commentHTML string) error {
	_, err := g.CreateCommentOnPageURL(ctx, projectID, pageURL, commentHTML)
	return err
}

// ListHumanSpecComments resolves the spec page at pageURL and returns the human
// comments on it (node H gate) — every comment that is not looper's own signed note
// and carries some text. Whether any of these is a real, unconditional approval is
// judged by the LLM at the runtime layer (comments are passed through verbatim), not
// by keyword matching here.
func (g *Gateway) ListHumanSpecComments(ctx context.Context, projectID, pageURL string) ([]PageComment, error) {
	pageID := PageIDFromURL(pageURL)
	if pageID == "" {
		return nil, fmt.Errorf("planedoc: ListHumanSpecComments cannot resolve page id from %q", pageURL)
	}
	comments, err := g.ListPageComments(ctx, projectID, pageID)
	if err != nil {
		return nil, err
	}
	human := make([]PageComment, 0, len(comments))
	for _, c := range comments {
		text := strings.TrimSpace(c.CommentStripped)
		if text == "" {
			text = strings.TrimSpace(c.CommentHTML)
		}
		if text == "" || commentIsLoopers(c) {
			continue // empty, or looper's own (signed) comment — not a human reply
		}
		human = append(human, c)
	}
	return human, nil
}

// projectLabel is a Plane project label (id + name), used to resolve a label name to
// the UUID a work item's label set stores.
type projectLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AddWorkItemLabel adds a label (by name) to a work item — how node H dispatches the
// worker: on approval it stamps looper:worker-ready so the worker discovers the item
// (node I). The label is created if the project lacks it; existing labels are
// preserved. Idempotent: a no-op when the work item already carries the label.
func (g *Gateway) AddWorkItemLabel(ctx context.Context, projectID, workItemID, labelName string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" || strings.TrimSpace(labelName) == "" {
		return fmt.Errorf("planedoc: AddWorkItemLabel requires project id, work item id, and label name")
	}
	labelID, err := g.resolveOrCreateLabel(ctx, projectID, labelName)
	if err != nil {
		return err
	}
	current, err := g.workItemLabelIDs(ctx, projectID, workItemID)
	if err != nil {
		return err
	}
	for _, id := range current {
		if id == labelID {
			return nil // already labelled
		}
	}
	updated := append(append([]string{}, current...), labelID)
	data := fmt.Sprintf(`{"labels":%s}`, jsonStringSlice(updated))
	args := append([]string{"api", "work-item", "update", "--project", projectID, workItemID, "--data", data}, g.globalArgs()...)
	if _, err := g.runPlane(ctx, "", args...); err != nil {
		return fmt.Errorf("planedoc: add work item label: %w", err)
	}
	return nil
}

// RemoveWorkItemLabel detaches a label (by name) from a work item, preserving the
// rest — how node H retires looper:plan once the spec is done, so the planner does not
// re-discover the item. Idempotent: a no-op when the project lacks the label or the
// work item doesn't carry it.
func (g *Gateway) RemoveWorkItemLabel(ctx context.Context, projectID, workItemID, labelName string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" || strings.TrimSpace(labelName) == "" {
		return fmt.Errorf("planedoc: RemoveWorkItemLabel requires project id, work item id, and label name")
	}
	listArgs := append([]string{"api", "label", "list", "--project", projectID, "--all", "--json"}, g.globalArgs()...)
	res, err := g.runPlane(ctx, "", listArgs...)
	if err != nil {
		return fmt.Errorf("planedoc: list labels: %w", err)
	}
	labelID := ""
	for _, l := range decodeLabelList(res.Stdout) {
		if strings.EqualFold(strings.TrimSpace(l.Name), strings.TrimSpace(labelName)) {
			labelID = l.ID
			break
		}
	}
	if labelID == "" {
		return nil // project doesn't have the label → nothing on the item to remove
	}
	current, err := g.workItemLabelIDs(ctx, projectID, workItemID)
	if err != nil {
		return err
	}
	next := make([]string, 0, len(current))
	found := false
	for _, id := range current {
		if id == labelID {
			found = true
			continue
		}
		next = append(next, id)
	}
	if !found {
		return nil // item doesn't carry it
	}
	data := fmt.Sprintf(`{"labels":%s}`, jsonStringSlice(next))
	args := append([]string{"api", "work-item", "update", "--project", projectID, workItemID, "--data", data}, g.globalArgs()...)
	if _, err := g.runPlane(ctx, "", args...); err != nil {
		return fmt.Errorf("planedoc: remove work item label: %w", err)
	}
	return nil
}

// SetWorkItemState moves a work item to the project workflow state named stateName
// (e.g. "In Progress" on dispatch, "In Review" when the worker opens a PR, "Done" on
// merge), resolving the name to its state UUID first. This mirrors the label-driven
// pipeline in Plane's own state column, which otherwise stays pinned at Todo. Reached
// through the `plane api request` escape hatch (the CLI has no typed `state` subcommand).
// Case-insensitive on the state name; errors when the project has no matching state so
// the caller can best-effort skip. Idempotent + harmless: re-PATCHing the same state
// is a no-op on Plane's side.
func (g *Gateway) SetWorkItemState(ctx context.Context, projectID, workItemID, stateName string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" || strings.TrimSpace(stateName) == "" {
		return fmt.Errorf("planedoc: SetWorkItemState requires project id, work item id, and state name")
	}
	ws := strings.TrimSpace(g.workspace)
	if ws == "" {
		return fmt.Errorf("planedoc: SetWorkItemState requires a workspace")
	}
	stateID, err := g.resolveStateID(ctx, projectID, stateName)
	if err != nil {
		return err
	}
	data := fmt.Sprintf(`{"state":%s}`, jsonString(stateID))
	path := fmt.Sprintf("workspaces/%s/projects/%s/issues/%s/", ws, projectID, workItemID)
	args := []string{"api", "request", path, "--method", "PATCH", "--data", data}
	args = append(args, g.globalArgs()...)
	if _, err := g.runPlane(ctx, "", args...); err != nil {
		return fmt.Errorf("planedoc: set work item state: %w", err)
	}
	return nil
}

// resolveStateID returns the UUID of the project workflow state named stateName
// (case-insensitive), listing the project's states through the request escape hatch.
// Errors when the project has no such state.
func (g *Gateway) resolveStateID(ctx context.Context, projectID, stateName string) (string, error) {
	ws := strings.TrimSpace(g.workspace)
	if ws == "" || strings.TrimSpace(projectID) == "" {
		return "", fmt.Errorf("planedoc: resolveStateID requires workspace and project id")
	}
	path := fmt.Sprintf("workspaces/%s/projects/%s/states/", ws, projectID)
	args := []string{"api", "request", path, "--method", "GET"}
	args = append(args, g.globalArgs()...)
	result, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return "", fmt.Errorf("planedoc: list states: %w", err)
	}
	for _, s := range decodeStates(result.Stdout) {
		if strings.EqualFold(strings.TrimSpace(s.Name), strings.TrimSpace(stateName)) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("planedoc: project %s has no state named %q", projectID, stateName)
}

// resolveOrCreateLabel returns the UUID of the project label named `name`, creating it
// if the project doesn't have one (case-insensitive match on the name).
func (g *Gateway) resolveOrCreateLabel(ctx context.Context, projectID, name string) (string, error) {
	listArgs := append([]string{"api", "label", "list", "--project", projectID, "--all", "--json"}, g.globalArgs()...)
	res, err := g.runPlane(ctx, "", listArgs...)
	if err != nil {
		return "", fmt.Errorf("planedoc: list labels: %w", err)
	}
	for _, l := range decodeLabelList(res.Stdout) {
		if strings.EqualFold(strings.TrimSpace(l.Name), strings.TrimSpace(name)) {
			return l.ID, nil
		}
	}
	createArgs := append([]string{"api", "label", "create", "--project", projectID, "--name", name, "--json"}, g.globalArgs()...)
	cres, err := g.runPlane(ctx, "", createArgs...)
	if err != nil {
		return "", fmt.Errorf("planedoc: create label: %w", err)
	}
	var created projectLabel
	if err := json.Unmarshal([]byte(strings.TrimSpace(cres.Stdout)), &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("planedoc: decode created label %q: %w", name, err)
	}
	return created.ID, nil
}

// workItemLabelIDs returns a work item's current label UUIDs. The API returns `labels`
// as an array of UUID strings (the shape the update endpoint takes); an array of {id}
// objects is tolerated defensively.
func (g *Gateway) workItemLabelIDs(ctx context.Context, projectID, workItemID string) ([]string, error) {
	args := append([]string{"api", "work-item", "get", "--project", projectID, workItemID, "--json"}, g.globalArgs()...)
	res, err := g.runPlane(ctx, "", args...)
	if err != nil {
		return nil, fmt.Errorf("planedoc: get work item: %w", err)
	}
	var wi struct {
		Labels json.RawMessage `json:"labels"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &wi); err != nil {
		return nil, fmt.Errorf("planedoc: decode work item: %w", err)
	}
	if len(wi.Labels) == 0 {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(wi.Labels, &ids); err == nil {
		return ids, nil
	}
	var objs []projectLabel
	if err := json.Unmarshal(wi.Labels, &objs); err == nil {
		for _, o := range objs {
			if strings.TrimSpace(o.ID) != "" {
				ids = append(ids, o.ID)
			}
		}
		return ids, nil
	}
	return nil, nil
}

// workItemState is a Plane project workflow state (id + name), used to resolve a
// state name (e.g. "In Progress") to the UUID the work-item update endpoint takes.
type workItemState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// decodeStates unwraps a bare array or {results:[...]} envelope of project states —
// the two shapes the states endpoint returns through the request escape hatch.
func decodeStates(stdout string) []workItemState {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil
	}
	var envelope struct {
		Results []workItemState `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err == nil && envelope.Results != nil {
		return envelope.Results
	}
	var bare []workItemState
	_ = json.Unmarshal([]byte(stdout), &bare)
	return bare
}

// decodeLabelList unwraps a bare array or {results:[...]} envelope of project labels.
func decodeLabelList(stdout string) []projectLabel {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil
	}
	var envelope struct {
		Results []projectLabel `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err == nil && envelope.Results != nil {
		return envelope.Results
	}
	var bare []projectLabel
	_ = json.Unmarshal([]byte(stdout), &bare)
	return bare
}

// jsonStringSlice JSON-encodes a string slice for a --data body ("[]" on error).
func jsonStringSlice(s []string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// SpecKind is which spec a dropped document is — decides the link title tag.
type SpecKind string

const (
	SpecKindProduct SpecKind = "product"
	SpecKindTech    SpecKind = "tech"
)

func (k SpecKind) linkTitle() (string, bool) {
	switch k {
	case SpecKindProduct:
		return ProductSpecLinkTitle, true
	case SpecKindTech:
		return TechSpecLinkTitle, true
	default:
		return "", false
	}
}

// AssociateDroppedSpec acts on an agent's judgment that a thread message is a spec
// (plan §8.3, "looper 主动关联"): people often won't create the Plane link
// themselves — they just drop the spec in the thread. Given a URL, it links it
// directly; given inline spec text, it first writes a Plane page (named `pageName`)
// then links that. Returns the associated URL. Idempotent via UpsertSpecLink.
func (g *Gateway) AssociateDroppedSpec(ctx context.Context, projectID, workItemID string, kind SpecKind, url, inlineText, pageName string) (string, error) {
	title, ok := kind.linkTitle()
	if !ok {
		return "", fmt.Errorf("planedoc: AssociateDroppedSpec unknown spec kind %q", kind)
	}
	url = strings.TrimSpace(url)
	inlineText = strings.TrimSpace(inlineText)
	switch {
	case url != "":
		// A link (Plane page / Feishu doc / any URL) — associate it as-is.
		if err := g.UpsertSpecLink(ctx, projectID, workItemID, title, url); err != nil {
			return "", err
		}
		return url, nil
	case inlineText != "":
		// Raw spec text pasted in the thread — capture it into a Plane page first.
		name := strings.TrimSpace(pageName)
		if name == "" {
			name = string(kind) + " spec"
		}
		page, err := g.CreatePage(ctx, projectID, name, inlineText)
		if err != nil {
			return "", err
		}
		if err := g.UpsertSpecLink(ctx, projectID, workItemID, title, page.URL); err != nil {
			return page.URL, err
		}
		return page.URL, nil
	default:
		return "", fmt.Errorf("planedoc: AssociateDroppedSpec needs a url or inline text")
	}
}

// WorkItemIDFromURL extracts a Plane work-item UUID from an issue/work-item URL of
// the form .../issues/<uuid> or .../work-items/<uuid>. This is how the planner /
// worker get the work-item UUID (which the link/comment/spec ops need) from the
// loop's issue URL, without an extra API round-trip. Returns "" when the URL isn't
// a Plane work-item URL.
func WorkItemIDFromURL(workItemURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(workItemURL), "/")
	for _, marker := range []string{"/issues/", "/work-items/"} {
		if i := strings.LastIndex(trimmed, marker); i >= 0 {
			id := trimmed[i+len(marker):]
			if id != "" && !strings.ContainsAny(id, "/?#") {
				return id
			}
		}
	}
	return ""
}

// pageWebURL constructs a page's human URL from the API base (Plane's page API
// returns none). Best-effort; the page id is embedded so it round-trips.
func (g *Gateway) pageWebURL(projectID, pageID string) string {
	base := strings.TrimSuffix(strings.TrimRight(g.apiBaseURL, "/"), "/api/v1")
	if base == "" {
		base = "https://plane.powerformer.net"
	}
	ws := g.workspace
	if ws == "" {
		return fmt.Sprintf("%s/pages/%s", base, pageID)
	}
	return fmt.Sprintf("%s/%s/projects/%s/pages/%s", base, ws, projectID, pageID)
}

// decodeLinks parses a `link list` response, tolerating both a bare array and the
// paginated {results:[...]} envelope the CLI emits.
func decodeLinks(stdout string) ([]Link, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil
	}
	var envelope struct {
		Results []Link `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err == nil && envelope.Results != nil {
		return envelope.Results, nil
	}
	var bare []Link
	if err := json.Unmarshal([]byte(stdout), &bare); err != nil {
		return nil, fmt.Errorf("planedoc: decode links: %w", err)
	}
	return bare, nil
}

// LooperCommentMarker is a legacy machine marker on looper's own comments; kept for
// backward-compatible detection. New comments carry the visible signature footer below.
const LooperCommentMarker = "[looper]"

// LooperSignatureMark is the stable substring inside the signature footer looper
// appends to every page comment it posts. It is both visible branding and the
// machine signal that a comment is looper's own — DetectSpecApproval treats any
// comment carrying it as NOT a human approval, closing the self-approve hole even
// when looper's Plane account is the same person as a human reviewer.
const LooperSignatureMark = "Powered by Looper"

// SignComment appends looper's signature footer to a page-comment body so the comment
// is both visibly branded ("🔁 Powered by Looper · runner=… · agent=… · An autonomous
// AI dev team for your GitHub repos.") and machine-distinguishable from a human reply.
// runner/agent add context (e.g. "reviewer"/"codex"); either may be empty.
func SignComment(bodyHTML, runner, agent string) string {
	parts := []string{"🔁 " + LooperSignatureMark}
	if r := strings.TrimSpace(runner); r != "" {
		parts = append(parts, "runner="+r)
	}
	if a := strings.TrimSpace(agent); a != "" {
		parts = append(parts, "agent="+a)
	}
	parts = append(parts, "An autonomous AI dev team for your GitHub repos.")
	return bodyHTML + "<p><i>" + htmlpkg.EscapeString(strings.Join(parts, " · ")) + "</i></p>"
}

// commentIsLoopers reports whether a page comment was posted by looper itself — it
// carries the signature footer or the legacy marker (in either the stripped text or
// the raw HTML).
func commentIsLoopers(c PageComment) bool {
	for _, s := range []string{c.CommentStripped, c.CommentHTML} {
		if strings.Contains(s, LooperSignatureMark) || strings.Contains(s, LooperCommentMarker) {
			return true
		}
	}
	return false
}

// decodePageComments unwraps either a bare array or a {results:[...]} envelope, the
// two shapes the Plane page-comments endpoint returns through the request escape hatch.
func decodePageComments(stdout string) ([]PageComment, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil
	}
	var envelope struct {
		Results []PageComment `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err == nil && envelope.Results != nil {
		return envelope.Results, nil
	}
	var bare []PageComment
	if err := json.Unmarshal([]byte(stdout), &bare); err != nil {
		return nil, fmt.Errorf("planedoc: decode page comments: %w", err)
	}
	return bare, nil
}

func decodeWorkItemComments(stdout string) ([]WorkItemComment, error) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return nil, nil
	}
	var envelope struct {
		Results []WorkItemComment `json:"results"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err == nil && envelope.Results != nil {
		return envelope.Results, nil
	}
	var bare []WorkItemComment
	if err := json.Unmarshal([]byte(stdout), &bare); err != nil {
		return nil, fmt.Errorf("planedoc: decode work item comments: %w", err)
	}
	return bare, nil
}

// jsonString safely JSON-encodes a string for embedding in a --data body.
func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
