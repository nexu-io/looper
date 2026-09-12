package hostingidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxResponseBytes = 1 << 20

// CheckAPIURL prevents a daemon transport from sending credentials to another
// origin or outside the selected forge's API path. Repository authorization is
// separately constrained by CheckRepository at the operation boundary.
func (session *Session) CheckAPIURL(raw string) error {
	target, err := url.Parse(raw)
	base, _ := url.Parse(session.apiURL)
	if err != nil || target.Opaque != "" || target.User != nil || target.Fragment != "" || target.RawFragment != "" || !strings.EqualFold(target.Scheme, base.Scheme) || !strings.EqualFold(target.Host, base.Host) {
		return session.failure("authorize request", "request URL is outside the selected API origin")
	}
	prefix := strings.TrimRight(base.Path, "/")
	if prefix != "" && target.Path != prefix && !strings.HasPrefix(target.Path, prefix+"/") {
		return session.failure("authorize request", "request URL is outside the selected API path")
	}
	for _, segment := range strings.Split(target.Path, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			return session.failure("authorize request", "request URL contains an invalid path")
		}
	}
	return nil
}

// CheckRepository restricts an operation to the configured owner/name.
func (session *Session) CheckRepository(repository string) error {
	if !strings.EqualFold(repository, session.resolved.Target.Repo) {
		return session.failure("authorize repository", "repository differs from the configured execution target")
	}
	return nil
}

func (session *Session) requestJSON(ctx context.Context, operation, method, path, authorization string, payload, result any) error {
	requestURL := session.apiURL + path
	if err := session.CheckAPIURL(requestURL); err != nil {
		return err
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return session.failure(operation, "cannot encode API request")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return session.failure(operation, "cannot construct API request")
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "looper-hosting-identity")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := session.manager.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return session.contextFailure(operation, ctx.Err())
		}
		return session.failure(operation, "hosting server request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		reason := "hosting server rejected the request"
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			reason = "hosting server redirects are not allowed"
		}
		if operation == "resolve Forgejo account" && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
			reason = "dedicated Forgejo token must be valid and include read:user permission"
		}
		return &Error{Identity: session.Name(), Operation: operation, StatusCode: response.StatusCode, Reason: reason}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return session.contextFailure(operation, ctx.Err())
		}
		return session.failure(operation, "cannot read hosting server response")
	}
	if len(data) > maxResponseBytes || json.Unmarshal(data, result) != nil {
		return session.failure(operation, "hosting server returned an invalid JSON response")
	}
	return nil
}

func (session *Session) verifyRepository(ctx context.Context, authorization string) error {
	parts := strings.Split(session.resolved.Target.Repo, "/")
	path := "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	var repository struct {
		FullName string `json:"full_name"`
	}
	if err := session.requestJSON(ctx, "verify repository access", http.MethodGet, path, authorization, nil, &repository); err != nil {
		return err
	}
	if !strings.EqualFold(repository.FullName, session.resolved.Target.Repo) {
		return session.failure("verify repository access", "hosting server returned a different repository")
	}
	return nil
}
