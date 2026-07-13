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
	ID   string
	Name string
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
	args := []string{"api", "page", "create", "--project", projectID, "--name", name, "--body", bodyMarkdown, "--format", "md", "--json"}
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
	trimmed := strings.TrimRight(strings.TrimSpace(pageURL), "/")
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
	return found, url, err
}

// CommentOnWorkItem posts an HTML comment on a Plane work item.
func (g *Gateway) CommentOnWorkItem(ctx context.Context, projectID, workItemID, commentHTML string) error {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(workItemID) == "" || strings.TrimSpace(commentHTML) == "" {
		return fmt.Errorf("planedoc: CommentOnWorkItem requires project id, work item id, and html")
	}
	data := fmt.Sprintf(`{"comment_html":%s}`, jsonString(commentHTML))
	args := []string{"api", "comment", "create", "--project", projectID, "--work-item", workItemID, "--data", data}
	args = append(args, g.globalArgs()...)
	if _, err := g.runPlane(ctx, "", args...); err != nil {
		return fmt.Errorf("planedoc: comment on work item: %w", err)
	}
	return nil
}

// RequestProductSpec asks the product owner (by an already-rendered mention/name)
// to supply a product spec, as a comment on the work item (flowchart node E, Plane
// side — the task-card @-mention in Feishu is a separate surface). The comment
// tells them looper will auto-associate whatever spec link/text they reply with.
func (g *Gateway) RequestProductSpec(ctx context.Context, projectID, workItemID, ownerMention, workItemName string) error {
	html := fmt.Sprintf(
		"<p>%s 这个需求「%s」还没有 product spec。请补一份 —— 直接把方案页链接或正文发在这里,looper 会自动把它关联到本 work item 并继续。</p>",
		htmlpkg.EscapeString(strings.TrimSpace(ownerMention)),
		htmlpkg.EscapeString(strings.TrimSpace(workItemName)),
	)
	return g.CommentOnWorkItem(ctx, projectID, workItemID, html)
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

// jsonString safely JSON-encodes a string for embedding in a --data body.
func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
