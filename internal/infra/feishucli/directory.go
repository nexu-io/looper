package feishucli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
)

// DirectoryUser contains notification identities, not login credentials. OpenID
// belongs to the application used by this client; it must not be reused by a
// different bot. Optional fields may be absent because of field permissions.
type DirectoryUser struct {
	OpenID        string   `json:"open_id"`
	UserID        string   `json:"user_id,omitempty"`
	UnionID       string   `json:"union_id,omitempty"`
	Name          string   `json:"name,omitempty"`
	EnglishName   string   `json:"en_name,omitempty"`
	Nickname      string   `json:"nickname,omitempty"`
	JobTitle      string   `json:"job_title,omitempty"`
	DepartmentIDs []string `json:"department_ids,omitempty"`
	Status        *struct {
		Frozen   bool `json:"is_frozen"`
		Resigned bool `json:"is_resigned"`
		Exited   bool `json:"is_exited"`
	} `json:"status,omitempty"`
}

func (c Client) ApplicationID(ctx context.Context) (string, error) {
	raw, err := c.run(ctx, nil, "auth", "status", "--json")
	if err != nil {
		return "", err
	}
	var status struct {
		AppID string `json:"appId"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", err
	}
	if status.AppID == "" {
		return "", fmt.Errorf("lark-cli has no configured appId")
	}
	return status.AppID, nil
}

// DirectoryUsers reads the app's authorized departments and individual users.
// It does not search by email/name or choose who should make a decision.
func (c Client) DirectoryUsers(ctx context.Context) ([]DirectoryUser, error) {
	var userIDs, departmentIDs []string
	var failures []error
	err := c.directoryPages(ctx, "/open-apis/contact/v3/scopes", nil, func(data json.RawMessage) error {
		var page struct {
			Users       []string `json:"user_ids"`
			Departments []string `json:"department_ids"`
			Groups      []string `json:"group_ids"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		userIDs = append(userIDs, page.Users...)
		departmentIDs = append(departmentIDs, page.Departments...)
		if len(page.Groups) > 0 {
			failures = append(failures, fmt.Errorf("contact scope contains user groups; directory currently reads authorized users and departments only"))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	users := map[string]DirectoryUser{}
	addUsers := func(data json.RawMessage) error {
		var page struct {
			Items []DirectoryUser `json:"items"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		for _, user := range page.Items {
			if user.OpenID != "" {
				users[user.OpenID] = user
			}
		}
		return nil
	}
	for start := 0; start < len(userIDs); start += 50 {
		ids := userIDs[start:min(start+50, len(userIDs))]
		data, err := c.directoryGet(ctx, "/open-apis/contact/v3/users/batch", map[string]any{"user_ids": ids})
		if err == nil {
			err = addUsers(data)
		}
		failures = append(failures, err)
	}
	seenDepartments := map[string]bool{}
	for _, root := range departmentIDs {
		departments := []string{root}
		err := c.directoryPages(ctx, "/open-apis/contact/v3/departments/"+url.PathEscape(root)+"/children", map[string]any{"fetch_child": true}, func(data json.RawMessage) error {
			var page struct {
				Items []struct {
					ID string `json:"open_department_id"`
				} `json:"items"`
			}
			if err := json.Unmarshal(data, &page); err != nil {
				return err
			}
			for _, dept := range page.Items {
				departments = append(departments, dept.ID)
			}
			return nil
		})
		failures = append(failures, err)
		for _, department := range departments {
			if department == "" || seenDepartments[department] {
				continue
			}
			seenDepartments[department] = true
			err := c.directoryPages(ctx, "/open-apis/contact/v3/users/find_by_department", map[string]any{"department_id": department}, addUsers)
			failures = append(failures, err)
		}
	}
	result := make([]DirectoryUser, 0, len(users))
	named := 0
	for _, user := range users {
		result = append(result, user)
		if user.Name != "" {
			named++
		}
	}
	if len(users) > 0 && named == 0 {
		failures = append(failures, fmt.Errorf("user names are unavailable; enable contact:user.base:readonly and publish the app"))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OpenID < result[j].OpenID })
	return result, errors.Join(failures...)
}

func (c Client) directoryGet(ctx context.Context, path string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["user_id_type"], params["department_id_type"] = "open_id", "open_department_id"
	query, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return c.callData(ctx, nil, "api", "GET", path, "--params", string(query), "--as", "bot")
}

func (c Client) directoryPages(ctx context.Context, path string, params map[string]any, consume func(json.RawMessage) error) error {
	if params == nil {
		params = map[string]any{}
	}
	params["page_size"] = 50
	seen := map[string]bool{}
	for {
		data, err := c.directoryGet(ctx, path, params)
		if err != nil {
			return err
		}
		if err := consume(data); err != nil {
			return err
		}
		var page struct {
			More  bool   `json:"has_more"`
			Token string `json:"page_token"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return err
		}
		if !page.More {
			return nil
		}
		if page.Token == "" || seen[page.Token] {
			return fmt.Errorf("incomplete Feishu directory page: %s", path)
		}
		seen[page.Token], params["page_token"] = true, page.Token
	}
}
