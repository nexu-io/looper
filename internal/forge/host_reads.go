package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/nexu-io/looper/internal/config"
)

type hostReadRoute struct {
	pattern *regexp.Regexp
	kind    config.ProviderKind
	list    bool
	field   string
	text    bool
	queries string
}

var hostReadRoutes = []hostReadRoute{
	{regexp.MustCompile(`^pulls$`), "", true, "", false, "state head base sort direction"},
	{regexp.MustCompile(`^(pulls|issues)/[1-9][0-9]*$`), "", false, "", false, ""},
	{regexp.MustCompile(`^issues/[1-9][0-9]*/comments$`), "", true, "", false, "since"},
	{regexp.MustCompile(`^pulls/[1-9][0-9]*/reviews$`), "", true, "", false, ""},
	{regexp.MustCompile(`^pulls/[1-9][0-9]*/reviews/[1-9][0-9]*/comments$`), "", true, "", false, ""},
	{regexp.MustCompile(`^pulls/[1-9][0-9]*/requested_reviewers$`), "", false, "", false, ""},
	{regexp.MustCompile(`^pulls/[1-9][0-9]*/(comments|files)$`), config.ProviderKindGitHub, true, "", false, "sort direction since"},
	{regexp.MustCompile(`^pulls/[1-9][0-9]*\.diff$`), config.ProviderKindForgejo, false, "", true, ""},
	{regexp.MustCompile(`^commits/[A-Za-z0-9][A-Za-z0-9._/-]*/check-runs$`), config.ProviderKindGitHub, true, "check_runs", false, "check_name status filter"},
	{regexp.MustCompile(`^commits/[A-Za-z0-9][A-Za-z0-9._/-]*/status$`), config.ProviderKindGitHub, false, "", false, ""},
	{regexp.MustCompile(`^commits/[A-Za-z0-9][A-Za-z0-9._/-]*/statuses$`), config.ProviderKindGitHub, true, "", false, ""},
	{regexp.MustCompile(`^statuses/[A-Za-z0-9][A-Za-z0-9._/-]*$`), config.ProviderKindForgejo, true, "", false, "sort state"},
	{regexp.MustCompile(`^actions/runs$`), "", true, "workflow_runs", false, "head_sha branch status"},
	{regexp.MustCompile(`^actions/runs/[1-9][0-9]*$`), "", false, "", false, ""},
	{regexp.MustCompile(`^actions/runs/[1-9][0-9]*/jobs$`), config.ProviderKindGitHub, true, "jobs", false, "filter"},
	{regexp.MustCompile(`^actions/runs/[1-9][0-9]*/jobs$`), config.ProviderKindForgejo, true, "", false, ""},
	{regexp.MustCompile(`^actions/jobs/[1-9][0-9]*/logs$`), "", false, "", true, "attempt"},
	{regexp.MustCompile(`^compare/[A-Za-z0-9][A-Za-z0-9._/-]*\.\.\.[A-Za-z0-9][A-Za-z0-9._/-]*$`), config.ProviderKindGitHub, false, "", false, ""},
}

func parseHostRead(req HostRequest, kind config.ProviderKind) (*url.URL, hostReadRoute, error) {
	if req.Method != "" && req.Method != http.MethodGet {
		return nil, hostReadRoute{}, errors.New("hosting API accepts only GET")
	}
	u, err := url.Parse(req.Path)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.ForceQuery || u.Path == "" || strings.HasPrefix(u.Path, "/") || strings.ContainsAny(u.EscapedPath(), "%\\") || len(req.Path) > 2048 {
		return nil, hostReadRoute{}, errors.New("hosting API requires a plain repository-relative path")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsFunc(part, unicode.IsControl) {
			return nil, hostReadRoute{}, errors.New("invalid hosting API path")
		}
	}
	queries, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, hostReadRoute{}, errors.New("invalid hosting API query")
	}
	for _, route := range hostReadRoutes {
		if route.kind != "" && route.kind != kind || !route.pattern.MatchString(u.Path) {
			continue
		}
		if req.Diff {
			if !regexp.MustCompile(`^pulls/[1-9][0-9]*$`).MatchString(u.Path) {
				return nil, route, errors.New("diff is supported only for one pull request")
			}
			route.text = true
			if kind == config.ProviderKindForgejo {
				u.Path += ".diff"
			}
		}
		if req.Paginate && !route.list {
			return nil, route, errors.New("pagination is unsupported for this endpoint")
		}
		allowed := " " + route.queries + " "
		if route.list {
			allowed += "page per_page limit "
		}
		for key, values := range queries {
			if !strings.Contains(allowed, " "+key+" ") || len(values) != 1 || len(values[0]) > 256 || strings.ContainsFunc(values[0], unicode.IsControl) {
				return nil, route, fmt.Errorf("unsupported hosting query parameter %q", key)
			}
			if key == "page" || key == "per_page" || key == "limit" || key == "attempt" {
				n, err := strconv.Atoi(values[0])
				if err != nil || n < 1 || n > 100 {
					return nil, route, errors.New("hosting pagination and attempt values must be between 1 and 100")
				}
			}
		}
		if kind == config.ProviderKindGitHub && queries.Has("limit") || kind == config.ProviderKindForgejo && queries.Has("per_page") {
			return nil, route, errors.New("pagination parameter does not match the hosting provider")
		}
		if req.Paginate && queries.Has("page") && queries.Get("page") != "1" {
			return nil, route, errors.New("complete pagination must begin at page 1")
		}
		u.RawQuery = queries.Encode()
		return u, route, nil
	}
	return nil, hostReadRoute{}, errors.New("unsupported hosting read endpoint")
}

func (b *hostBroker) readAPI(ctx context.Context, req HostRequest) ([]byte, error) {
	u, route, err := parseHostRead(req, b.session.Target().Kind)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(b.session.Target().Repo, "/")
	prefix := "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/"
	queries := u.Query()
	pageSize := 100
	pageKey := "per_page"
	if b.session.Target().Kind == config.ProviderKindForgejo {
		pageKey = "limit"
		pageSize = 50
	}
	if value := queries.Get(pageKey); value != "" {
		pageSize, _ = strconv.Atoi(value)
	}
	if req.Paginate {
		queries.Set(pageKey, strconv.Itoa(pageSize))
		queries.Set("page", "1")
	}
	var combined []json.RawMessage
	var envelope map[string]json.RawMessage
	totalBytes := 0
	for page := 1; page <= maxHostPages; page++ {
		u.RawQuery = queries.Encode()
		accept := "application/json"
		if route.text {
			accept = "text/plain"
		}
		if req.Diff && b.session.Target().Kind == config.ProviderKindGitHub {
			accept = "application/vnd.github.diff"
		}
		data, headers, err := b.request(ctx, http.MethodGet, prefix+u.String(), nil, accept)
		if err != nil {
			return nil, err
		}
		totalBytes += len(data)
		if totalBytes > maxHostResponseBytes {
			return nil, errors.New("paginated hosting response exceeds size limit")
		}
		if route.text {
			return data, nil
		}
		if !json.Valid(data) {
			return nil, errors.New("hosting server returned invalid JSON")
		}
		if !req.Paginate {
			return data, nil
		}
		items := data
		if route.field != "" {
			var current map[string]json.RawMessage
			if err := json.Unmarshal(data, &current); err != nil {
				return nil, errors.New("hosting response collection is invalid")
			}
			items = current[route.field]
			if envelope == nil {
				envelope = current
			}
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(items, &entries); err != nil || !bytes.HasPrefix(bytes.TrimSpace(items), []byte("[")) {
			return nil, errors.New("hosting response collection is invalid")
		}
		combined = append(combined, entries...)
		next, err := b.nextPage(headers, prefix+u.Path, queries, page)
		if err != nil {
			return nil, err
		}
		if !next && (len(entries) < pageSize || headers.Get("Link") != "" || headers.Get("X-Total-Pages") != "") {
			return encodeHostCollection(envelope, route.field, combined)
		}
		// A server that omits Link can still expose full pages. Fetch one more
		// page to distinguish a complete collection from silent truncation.
		if !next && len(entries) == 0 {
			return encodeHostCollection(envelope, route.field, combined)
		}
		queries.Set("page", strconv.Itoa(page+1))
	}
	return nil, errors.New("hosting pagination exceeds page limit")
}

func encodeHostCollection(envelope map[string]json.RawMessage, field string, items []json.RawMessage) ([]byte, error) {
	if items == nil {
		items = []json.RawMessage{}
	}
	array, err := json.Marshal(items)
	if err != nil || field == "" {
		return array, err
	}
	envelope[field] = array
	return json.Marshal(envelope)
}

func (b *hostBroker) nextPage(headers http.Header, path string, query url.Values, page int) (bool, error) {
	for _, link := range strings.Split(headers.Get("Link"), ",") {
		if !strings.Contains(link, `rel="next"`) && !strings.Contains(link, "rel=next") {
			continue
		}
		left, right := strings.IndexByte(link, '<'), strings.IndexByte(link, '>')
		if left < 0 || right <= left {
			return false, errors.New("invalid hosting pagination link")
		}
		u, err := url.Parse(link[left+1 : right])
		if err != nil {
			return false, errors.New("invalid hosting pagination link")
		}
		base, _ := url.Parse(b.session.APIURL())
		if !u.IsAbs() {
			u = base.ResolveReference(u)
		}
		if err := b.session.CheckAPIURL(u.String()); err != nil {
			return false, errors.New("hosting pagination points outside the bound API")
		}
		if u.Path != strings.TrimRight(base.Path, "/")+path {
			return false, errors.New("hosting pagination changed endpoint or repository")
		}
		nextQuery := u.Query()
		if nextQuery.Get("page") != strconv.Itoa(page+1) {
			return false, errors.New("hosting pagination has an invalid next page")
		}
		current := url.Values{}
		for key, values := range query {
			current[key] = append([]string(nil), values...)
		}
		current.Set("page", strconv.Itoa(page+1))
		if current.Encode() != nextQuery.Encode() {
			return false, errors.New("hosting pagination changed query scope")
		}
		return true, nil
	}
	if value := headers.Get("X-Total-Pages"); value != "" {
		total, err := strconv.Atoi(value)
		if err != nil || total < 0 || total > maxHostPages {
			return false, errors.New("hosting pagination exceeds page limit or is invalid")
		}
		return page < total, nil
	}
	return false, nil
}
