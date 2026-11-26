package forge

import (
	"context"
	"github.com/nexu-io/looper/internal/planestrict"
	"testing"
)

func TestSignedCollaborationIssueUsesWebOriginWithoutAnotherAPIToken(t *testing.T) {
	client := &PlaneClient{webBaseURL: "https://plane.example"}
	d := planestrict.Dispatch{ID: "dispatch", RequestedMode: "collab", Issue: &planestrict.DispatchIssue{Title: "Cross-repository task", Body: "<p>Investigate first</p>", Number: 42, Path: "/team/browse/TEST-42"}}
	issue, err := client.StrictDispatchIssue(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Title != d.Issue.Title || issue.Number != 42 || issue.HTMLURL != "https://plane.example/team/browse/TEST-42" || issue.StrictDispatchID != "dispatch" {
		t.Fatalf("signed task lost identity: %#v", issue)
	}
}
