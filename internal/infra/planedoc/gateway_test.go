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
		!argPairPresent(args, "--body", "# Body\n- x") ||
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
	f := &fakeRun{stdouts: []string{`{"results":[{"id":"l1","title":"looper:product-spec","url":"https://x/pages/p1"}]}`}}
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
}

func TestRequestProductSpecCommentsWithEscapedMention(t *testing.T) {
	f := &fakeRun{stdouts: []string{`{"id":"c1"}`}}
	g := newGateway(f)
	if err := g.RequestProductSpec(context.Background(), "p1", "wi-1", "@产品<x>", "登录 & 注册"); err != nil {
		t.Fatalf("RequestProductSpec error = %v", err)
	}
	args := f.calls[0]
	if !argsContain(args, "api", "comment", "create") || !argPairPresent(args, "--work-item", "wi-1") {
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
