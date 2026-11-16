package feishucli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func directoryReply(data any) ([]byte, error) {
	return json.Marshal(map[string]any{"ok": true, "data": data})
}

func TestDirectoryUsersReadsAuthorizedScopeAndAllPagesAsBot(t *testing.T) {
	requests := map[string]int{}
	client := Client{Run: func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
		if len(args) != 7 || args[0] != "api" || args[1] != "GET" || args[5] != "--as" || args[6] != "bot" {
			t.Fatalf("directory did not use bot-only GET: %v", args)
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(args[4]), &params); err != nil {
			t.Fatal(err)
		}
		if params["user_id_type"] != "open_id" || params["department_id_type"] != "open_department_id" {
			t.Fatalf("incorrect identity types: %v", params)
		}
		key := args[2]
		requests[key]++
		switch key {
		case "/open-apis/contact/v3/scopes":
			if params["page_token"] == "next-scope" {
				return directoryReply(map[string]any{"department_ids": []string{"od-root"}})
			}
			return directoryReply(map[string]any{"user_ids": []string{"ou_direct"}, "has_more": true, "page_token": "next-scope"})
		case "/open-apis/contact/v3/users/batch":
			return directoryReply(map[string]any{"items": []DirectoryUser{{OpenID: "ou_direct", Name: "直接授权同学"}}})
		case "/open-apis/contact/v3/departments/od-root/children":
			if params["fetch_child"] != true {
				t.Fatal("nested departments not requested")
			}
			return directoryReply(map[string]any{"items": []map[string]string{{"open_department_id": "od-child"}}})
		case "/open-apis/contact/v3/users/find_by_department":
			if params["department_id"] == "od-child" {
				return directoryReply(map[string]any{"items": []DirectoryUser{{OpenID: "ou_new", Name: "新入职同学"}}})
			}
			if params["page_token"] == "next-user" {
				return directoryReply(map[string]any{"items": []DirectoryUser{{OpenID: "ou_direct", Name: "直接授权同学"}}})
			}
			return directoryReply(map[string]any{"items": []DirectoryUser{{OpenID: "ou_first", Name: "同事"}}, "has_more": true, "page_token": "next-user"})
		default:
			t.Fatalf("unexpected directory request: %v", args)
			return nil, nil
		}
	}}
	users, err := client.DirectoryUsers(context.Background())
	if err != nil || len(users) != 3 || users[2].OpenID != "ou_new" {
		t.Fatalf("incomplete/deduplication failure: %+v, %v", users, err)
	}
	if requests["/open-apis/contact/v3/users/find_by_department"] != 3 {
		t.Fatalf("missing a department/user page: %v", requests)
	}
}

func TestDirectoryReportsIncompletePagesAndFieldPermissions(t *testing.T) {
	for _, mode := range []string{"missing_names", "permission_denied", "repeated_page", "groups"} {
		t.Run(mode, func(t *testing.T) {
			client := Client{Run: func(_ context.Context, _ string, _ []byte, args ...string) ([]byte, error) {
				if mode == "permission_denied" {
					return nil, fmt.Errorf("missing contact:contact.base:readonly")
				}
				if args[2] == "/open-apis/contact/v3/scopes" {
					if mode == "groups" {
						return directoryReply(map[string]any{"group_ids": []string{"g1"}})
					}
					if mode == "repeated_page" {
						return directoryReply(map[string]any{"has_more": true, "page_token": "same"})
					}
					return directoryReply(map[string]any{"user_ids": []string{"ou_hidden_name"}})
				}
				return directoryReply(map[string]any{"items": []DirectoryUser{{OpenID: "ou_hidden_name"}}})
			}}
			_, err := client.DirectoryUsers(context.Background())
			if err == nil {
				t.Fatalf("%s silently treated as a complete directory", mode)
			}
			if mode == "missing_names" && !strings.Contains(err.Error(), "contact:user.base:readonly") {
				t.Fatal(err)
			}
		})
	}
}
