package planedoc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexu-io/looper/internal/infra/shell"
)

// fakeRun records each invocation's args and returns a scripted stdout per call.
type fakeRun struct {
	calls   [][]string
	stdouts []string
}

func (f *fakeRun) run(_ context.Context, o shell.Options) (shell.Result, error) {
	f.calls = append(f.calls, o.Args)
	out := ""
	if len(f.stdouts) >= len(f.calls) {
		out = f.stdouts[len(f.calls)-1]
	}
	return shell.Result{Stdout: out}, nil
}

func newGateway(f *fakeRun) *Gateway {
	return New(Options{
		PlanePath:  "plane",
		APIBaseURL: "https://plane.powerformer.net/api/v1",
		APIKey:     "secret-key",
		Workspace:  "open-design",
		Run:        f.run,
	})
}

func argsContain(args []string, sub ...string) bool {
	joined := strings.Join(args, "\x00")
	for _, s := range sub {
		if !strings.Contains(joined, s) {
			return false
		}
	}
	return true
}

// argPairPresent checks that flag is immediately followed by value.
func argPairPresent(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestCreatePageBuildsArgsAndParsesID(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"id":"pg-1","name":"Tech Spec"}`}}
	g := newGateway(f)
	page, err := g.CreatePage(context.Background(), "proj-1", "Tech Spec", "# Body\n- x")
	if err != nil {
		t.Fatalf("CreatePage error = %v", err)
	}
	if page.ID != "pg-1" || page.Name != "Tech Spec" {
		t.Fatalf("page = %+v, want id pg-1", page)
	}
	if !strings.Contains(page.URL, "pg-1") || !strings.Contains(page.URL, "open-design") {
		t.Fatalf("page URL = %q, want it to embed the page id + workspace", page.URL)
	}
	args := f.calls[0]
	if !argsContain(args, "api", "page", "create", "--json") ||
		!argPairPresent(args, "--project", "proj-1") ||
		!argPairPresent(args, "--name", "Tech Spec") ||
		!argsContain(args, "--body=# Body\n- x") ||
		!argPairPresent(args, "--api-key", "secret-key") ||
		!argPairPresent(args, "--workspace", "open-design") {
		t.Fatalf("create args = %v, missing expected flags", args)
	}
}

func TestPageContentReturnsBody(t *testing.T) {
	f := &fakeRun{stdouts: []string{"<h1>Spec</h1>\n"}}
	g := newGateway(f)
	html, err := g.PageContent(context.Background(), "proj-1", "pg-1")
	if err != nil {
		t.Fatalf("PageContent error = %v", err)
	}
	if html != "<h1>Spec</h1>" {
		t.Fatalf("content = %q, want trimmed html", html)
	}
	if !argsContain(f.calls[0], "api", "page", "get", "--content") || !argsContain(f.calls[0], "pg-1") {
		t.Fatalf("get args = %v", f.calls[0])
	}
}

func TestPageDocumentReturnsContentAndProvenance(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"id":"pg-1","name":"Product spec","description_html":"<p>Scope</p>","created_by":"owner","updated_by":"editor","owned_by":"owner"}`}}
	page, err := newGateway(f).PageDocument(context.Background(), "proj-1", "pg-1")
	if err != nil {
		t.Fatalf("PageDocument error = %v", err)
	}
	if page.ContentHTML != "<p>Scope</p>" || !page.AuthoredBy("owner") || !page.AuthoredBy("editor") || page.AuthoredBy("someone-else") {
		t.Fatalf("page provenance = %+v", page)
	}
	if !argsContain(f.calls[0], "api", "page", "get", "--json") || argsContain(f.calls[0], "--content") {
		t.Fatalf("get args = %v", f.calls[0])
	}
}

func TestFindSpecLinkFiltersByTitle(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://x/pages/p9"},{"id":"l2","title":"other","url":"https://y"}]}`}}
	g := newGateway(f)
	url, found, err := g.FindSpecLink(context.Background(), "proj-1", "wi-1", ProductSpecLinkTitle)
	if err != nil || !found || url != "https://x/pages/p9" {
		t.Fatalf("FindSpecLink = %q, %v, %v; want the product-spec url", url, found, err)
	}
	// A tag with no matching link returns not-found, no error.
	if _, found, err := g.FindSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle); err != nil || found {
		t.Fatalf("FindSpecLink(tech) = found %v, err %v; want not found", found, err)
	}
}

func TestUpsertSpecLinkCreatesWhenAbsent(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[]}`, `{"id":"l-new"}`}}
	g := newGateway(f)
	if err := g.UpsertSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle, "https://x/pages/p1"); err != nil {
		t.Fatalf("UpsertSpecLink error = %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want list + create", len(f.calls))
	}
	create := f.calls[1]
	if !argsContain(create, "api", "link", "create") || !argPairPresent(create, "--work-item", "wi-1") {
		t.Fatalf("create args = %v", create)
	}
	// The --data payload carries both url and title.
	if !argsContain(create, `"url":"https://x/pages/p1"`, `"title":"looper:tech-spec"`) {
		t.Fatalf("create --data missing url/title: %v", create)
	}
}

func TestUpsertSpecLinkNoopWhenSameURL(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://x/pages/p1"}]}`}}
	g := newGateway(f)
	if err := g.UpsertSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle, "https://x/pages/p1"); err != nil {
		t.Fatalf("UpsertSpecLink error = %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want only the list (no-op on identical link)", len(f.calls))
	}
}

func TestUpsertSpecLinkUpdatesStaleURL(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://x/OLD"}]}`, `{}`}}
	g := newGateway(f)
	if err := g.UpsertSpecLink(context.Background(), "proj-1", "wi-1", TechSpecLinkTitle, "https://x/NEW"); err != nil {
		t.Fatalf("UpsertSpecLink error = %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want list + update", len(f.calls))
	}
	update := f.calls[1]
	if !argsContain(update, "api", "link", "update", "l1", `"url":"https://x/NEW"`) {
		t.Fatalf("update args = %v", update)
	}
}

func TestPageIDFromURL(t *testing.T) {
	cases := map[string]string{
		"https://plane.powerformer.net/open-design/projects/p1/pages/abc-123":  "abc-123",
		"https://plane.powerformer.net/open-design/projects/p1/pages/abc-123/": "abc-123",
		"https://feishu.cn/docs/some-doc":                                      "", // human dropped a non-Plane link
		"https://plane.x/pages/id?tab=1":                                       "", // query → not a clean page id
		"":                                                                     "",
	}
	for url, want := range cases {
		if got := PageIDFromURL(url); got != want {
			t.Fatalf("PageIDFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestWorkItemIDFromURL(t *testing.T) {
	cases := map[string]string{
		"https://plane.powerformer.net/open-design/projects/p1/issues/wi-uuid-9":      "wi-uuid-9",
		"https://plane.powerformer.net/open-design/projects/p1/work-items/wi-uuid-9/": "wi-uuid-9",
		"https://github.com/owner/repo/issues/42":                                     "42", // generic /issues/ still parses trailing id
		"https://plane.x/projects/p1":                                                 "",
		"":                                                                            "",
	}
	for url, want := range cases {
		if got := WorkItemIDFromURL(url); got != want {
			t.Fatalf("WorkItemIDFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestReadSpecResolvesLinkToPageContent(t *testing.T) {
	// list links → find tech-spec URL → page get --content
	f := &fakeRun{stdouts: []string{
		`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://plane.x/open-design/projects/p1/pages/pg-9"}]}`,
		"<h1>Tech</h1>",
	}}
	g := newGateway(f)
	content, found, err := g.ReadSpec(context.Background(), "p1", "wi-1", TechSpecLinkTitle)
	if err != nil || !found || content != "<h1>Tech</h1>" {
		t.Fatalf("ReadSpec = %q, %v, %v", content, found, err)
	}
	if !argsContain(f.calls[1], "api", "page", "get", "pg-9", "--content") {
		t.Fatalf("page get args = %v", f.calls[1])
	}
}

func TestReadSpecReturnsRawURLForNonPageLink(t *testing.T) {
	// A human dropped a Feishu doc — no Plane page to read, but the association exists.
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://feishu.cn/docs/xyz"}]}`}}
	g := newGateway(f)
	content, found, err := g.ReadSpec(context.Background(), "p1", "wi-1", ProductSpecLinkTitle)
	if err != nil || !found || content != "https://feishu.cn/docs/xyz" {
		t.Fatalf("ReadSpec(non-page) = %q, %v, %v; want the raw url", content, found, err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want only the link list (no page get for a non-page link)", len(f.calls))
	}
}

func TestWriteTechSpecCreatesPageAndLinks(t *testing.T) {
	// page create → link list (empty) → link create
	f := &fakeRun{stdouts: []string{`{"id":"pg-1","name":"Tech"}`, `{"results":[]}`, `{"id":"l-new"}`}}
	g := newGateway(f)
	page, err := g.WriteTechSpec(context.Background(), "p1", "wi-1", "Tech", "# spec")
	if err != nil || page.ID != "pg-1" {
		t.Fatalf("WriteTechSpec = %+v, %v", page, err)
	}
	if len(f.calls) != 3 || !argsContain(f.calls[0], "page", "create") || !argsContain(f.calls[2], "link", "create") {
		t.Fatalf("calls = %d: %v", len(f.calls), f.calls)
	}
}

func TestAssociateDroppedSpecLinksAURL(t *testing.T) {
	// Human dropped a doc link in the thread → link it directly (list empty → create).
	f := &fakeRun{stdouts: []string{`{"results":[]}`, `{"id":"l-new"}`}}
	g := newGateway(f)
	url, err := g.AssociateDroppedSpec(context.Background(), "p1", "wi-1", SpecKindProduct, "https://feishu.cn/docs/xyz", "", "")
	if err != nil || url != "https://feishu.cn/docs/xyz" {
		t.Fatalf("AssociateDroppedSpec(url) = %q, %v", url, err)
	}
	if len(f.calls) != 2 || !argsContain(f.calls[1], "link", "create", `"title":"looper:product-spec"`) {
		t.Fatalf("calls = %v; want list + product-spec link create", f.calls)
	}
}

func TestAssociateDroppedSpecWritesPageForInlineText(t *testing.T) {
	// Human pasted raw spec text → write a Plane page first, then link it.
	f := &fakeRun{stdouts: []string{`{"id":"pg-7","name":"prod"}`, `{"results":[]}`, `{"id":"l-new"}`}}
	g := newGateway(f)
	url, err := g.AssociateDroppedSpec(context.Background(), "p1", "wi-1", SpecKindProduct, "", "# 需求\n验收标准…", "产品方案 #9")
	if err != nil || !strings.Contains(url, "pg-7") {
		t.Fatalf("AssociateDroppedSpec(inline) = %q, %v", url, err)
	}
	if len(f.calls) != 3 || !argsContain(f.calls[0], "page", "create") || !argsContain(f.calls[2], "link", "create") {
		t.Fatalf("calls = %v; want page create + list + link create", f.calls)
	}
}

func TestAssociateDroppedSpecRejectsEmptyAndUnknownKind(t *testing.T) {
	g := newGateway(&fakeRun{})
	if _, err := g.AssociateDroppedSpec(context.Background(), "p1", "wi-1", SpecKindTech, "", "", ""); err == nil {
		t.Fatal("want error when neither url nor inline text is given")
	}
	if _, err := g.AssociateDroppedSpec(context.Background(), "p1", "wi-1", SpecKind("bogus"), "https://x", "", ""); err == nil {
		t.Fatal("want error for unknown spec kind")
	}
}

func TestDecideIntakeAndHasProductSpec(t *testing.T) {
	if DecideIntake(true) != IntakeProceed {
		t.Fatal("with a product spec → proceed")
	}
	if DecideIntake(false) != IntakeRequestProduct {
		t.Fatal("no product spec → request from product")
	}
	// HasProductSpec finds the product-spec link.
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://x/pages/p1"}]}`, "# Product Spec"}}
	g := newGateway(f)
	present, url, err := g.HasProductSpec(context.Background(), "p1", "wi-1")
	if err != nil || !present || url != "https://x/pages/p1" {
		t.Fatalf("HasProductSpec = %v, %q, %v", present, url, err)
	}
	// none → absent
	f2 := &fakeRun{stdouts: []string{`{"results":[]}`}}
	if present, _, _ := newGateway(f2).HasProductSpec(context.Background(), "p1", "wi-1"); present {
		t.Fatal("HasProductSpec = true, want false for a work item with no product-spec link")
	}
	for name, outputs := range map[string][]string{
		"external": {`{"results":[{"title":"looper:product-spec","url":"https://feishu.example/doc/1"}]}`},
		"blank":    {`{"results":[{"title":"looper:product-spec","url":"https://x/pages/p1"}]}`, "   "},
	} {
		if present, _, _ := newGateway(&fakeRun{stdouts: outputs}).HasProductSpec(context.Background(), "p1", "wi-1"); present {
			t.Fatalf("HasProductSpec = true for %s formal Spec", name)
		}
	}
}

func TestWorkItemCommentsPreserveUUIDActorAndTime(t *testing.T) {
	f := &fakeRun{stdouts: []string{
		`{"results":[{"id":"comment-uuid","actor":"member-uuid","comment_html":"<p>ENG-001: A</p>","created_at":"2026-07-17T10:00:00Z","updated_at":"2026-07-17T10:01:00Z"}]}`,
		`{"id":"created-uuid","actor":"bot-uuid","comment_html":"<p>ask</p>","created_at":"2026-07-17T10:02:00Z"}`,
	}}
	g := newGateway(f)
	comments, err := g.ListWorkItemComments(context.Background(), "proj-1", "wi-1")
	if err != nil || len(comments) != 1 || comments[0].ID != "comment-uuid" || comments[0].Actor != "member-uuid" || comments[0].CreatedAt == "" {
		t.Fatalf("ListWorkItemComments = %#v, %v", comments, err)
	}
	created, err := g.CreateWorkItemComment(context.Background(), "proj-1", "wi-1", "<p>ask</p>")
	if err != nil || created.ID != "created-uuid" || created.CreatedAt == "" {
		t.Fatalf("CreateWorkItemComment = %#v, %v", created, err)
	}
	if !argsContain(f.calls[0], "workspaces/open-design/projects/proj-1/work-items/wi-1/comments/", "--method", "GET") || !argsContain(f.calls[1], "--method", "POST") {
		t.Fatalf("calls = %#v", f.calls)
	}
}

func TestRequestProductSpecCommentsWithEscapedMention(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"results":[]}`, `{"id":"c1"}`}}
	g := newGateway(f)
	comment, err := g.RequestProductSpec(context.Background(), "p1", "wi-1", "@产品<x>", "登录 & 注册")
	if err != nil {
		t.Fatalf("RequestProductSpec error = %v", err)
	}
	if comment.ID != "c1" {
		t.Fatalf("comment id = %q, want c1", comment.ID)
	}
	if len(f.calls) != 2 || !argsContain(f.calls[0], "api", "request", "workspaces/open-design/projects/p1/work-items/wi-1/comments/", "--method", "GET") {
		t.Fatalf("calls = %v, want list then create", f.calls)
	}
	args := f.calls[1]
	if !argsContain(args, "api", "request", "workspaces/open-design/projects/p1/work-items/wi-1/comments/", "--method", "POST") {
		t.Fatalf("comment args = %v", args)
	}
	// The --data value is valid JSON whose decoded comment_html has the mention/name
	// HTML-escaped (so a "<x>" injection can't become raw markup).
	var data struct {
		CommentHTML string `json:"comment_html"`
	}
	if err := json.Unmarshal([]byte(argValue(args, "--data")), &data); err != nil {
		t.Fatalf("--data not valid JSON: %v", err)
	}
	if !strings.Contains(data.CommentHTML, "&lt;x&gt;") || !strings.Contains(data.CommentHTML, "&amp;") {
		t.Fatalf("comment_html not escaped: %q", data.CommentHTML)
	}
	for _, want := range []string{"用户问题与目标", "首版范围和非目标", "验收标准", "方案链接/正文", "验证产品身份"} {
		if !strings.Contains(data.CommentHTML, want) {
			t.Fatalf("product Spec instruction missing %q: %q", want, data.CommentHTML)
		}
	}
}

func TestRequestProductSpecReusesExactExistingAsk(t *testing.T) {
	html := `<p>产品负责人 请先为需求「导出」补一份可执行的 product spec，再让 looper 开始技术梳理。</p><p>至少写清：用户问题与目标、首版范围和非目标、关键交互或输出、验收标准；涉及付费策略或阶段优先级，也请直接在 spec 中定下来。</p><p>请由产品负责人创建或更新方案页，或由产品负责人在这条评论下明确回复方案链接/正文。Looper 不会代写产品范围；验证产品身份后才会关联到本 work item 并继续。</p>`
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"existing","comment_html":` + jsonString(html) + `}]}`}}
	comment, err := newGateway(f).RequestProductSpec(context.Background(), "p1", "wi-1", "产品负责人", "导出")
	if err != nil {
		t.Fatalf("RequestProductSpec error = %v", err)
	}
	if comment.ID != "existing" || len(f.calls) != 1 {
		t.Fatalf("comment = %+v, calls = %v; want one list call reusing existing ask", comment, f.calls)
	}
}

func TestDroppedSpecContentPrefersLinkAndPreservesInlineText(t *testing.T) {
	url, inline := DroppedSpecContent(WorkItemComment{CommentHTML: `<p>方案：<a href="https://docs.example/spec?a=1&amp;b=2">查看</a></p>`})
	if url != "https://docs.example/spec?a=1&b=2" || inline != "" {
		t.Fatalf("linked content = (%q, %q)", url, inline)
	}
	url, inline = DroppedSpecContent(WorkItemComment{CommentHTML: `<p>目标：高保真导出</p><p>验收：React 与 CSS 分离</p>`})
	if url != "" || inline != "目标：高保真导出\n验收：React 与 CSS 分离" {
		t.Fatalf("inline content = (%q, %q)", url, inline)
	}
}

func argValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func TestDecodeLinksToleratesBareArray(t *testing.T) {
	links, err := decodeLinks(`[{"id":"l1","title":"t","url":"u"}]`)
	if err != nil || len(links) != 1 || links[0].ID != "l1" {
		t.Fatalf("decodeLinks(bare) = %v, %v", links, err)
	}
	if links, err := decodeLinks("  "); err != nil || links != nil {
		t.Fatalf("decodeLinks(empty) = %v, %v", links, err)
	}
}

func TestSetWorkItemStateResolvesNameToIDAndPatches(t *testing.T) {
	// GET states (envelope shape) then PATCH the issue with the resolved state id.
	// Name match is case-insensitive ("in progress" → "In Progress").
	f := &fakeRun{stdouts: []string{
		`{"results":[{"id":"st-todo","name":"Todo"},{"id":"st-prog","name":"In Progress"},{"id":"st-review","name":"In Review"}]}`,
		`{}`,
	}}
	g := newGateway(f)
	if err := g.SetWorkItemState(context.Background(), "proj-1", "wi-1", "in progress"); err != nil {
		t.Fatalf("SetWorkItemState error = %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want list states + patch", len(f.calls))
	}
	get := f.calls[0]
	if !argsContain(get, "api", "request", "workspaces/open-design/projects/proj-1/states/") ||
		!argPairPresent(get, "--method", "GET") {
		t.Fatalf("get states args = %v", get)
	}
	patch := f.calls[1]
	if !argsContain(patch, "api", "request", "workspaces/open-design/projects/proj-1/issues/wi-1/") ||
		!argPairPresent(patch, "--method", "PATCH") ||
		!argsContain(patch, `"state":"st-prog"`) {
		t.Fatalf("patch args = %v", patch)
	}
}

func TestSetWorkItemStateToleratesBareArrayAndErrorsOnUnknownState(t *testing.T) {
	// States returned as a bare array (not enveloped); the requested state is absent →
	// error, and NO PATCH is issued (the caller best-effort skips on the error).
	f := &fakeRun{stdouts: []string{`[{"id":"st-todo","name":"Todo"}]`}}
	g := newGateway(f)
	if err := g.SetWorkItemState(context.Background(), "proj-1", "wi-1", "Done"); err == nil {
		t.Fatal("SetWorkItemState error = nil, want error for missing state")
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want only the states list (no PATCH on unresolved state)", len(f.calls))
	}
}

func TestCommentIsLoopers(t *testing.T) {
	// looper's own comments — carried by the legacy [looper] marker OR the signature
	// footer — are excluded from the human comments ListHumanSpecComments hands to the
	// LLM judge (even when looper's Plane account shares a human display name, e.g. mashu).
	if !commentIsLoopers(PageComment{CommentStripped: "[looper] node H 辅助审:第2节缺验收标准", DisplayName: "mashu"}) {
		t.Fatal("legacy [looper] marker must be recognized as looper's own")
	}
	signed := PageComment{ID: "5", CommentStripped: "方案没问题,同意 🔁 " + LooperSignatureMark + " · runner=reviewer · An autonomous AI dev team for your GitHub repos.", DisplayName: "mashu"}
	if !commentIsLoopers(signed) {
		t.Fatal("a signed (Powered by Looper) comment must be recognized as looper's own")
	}
	// A genuine unsigned human comment is not looper's — it reaches the LLM judge.
	if commentIsLoopers(PageComment{CommentStripped: "看过了,同意", DisplayName: "杨瑾龙"}) {
		t.Fatal("an unsigned human comment must not be treated as looper's own")
	}
}
