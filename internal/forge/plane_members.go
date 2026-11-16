package forge

import (
	"context"
	"net/http"
)

// PlaneMember is a person in this Plane project. IDs identify people, not their
// product/design/QA responsibility on a particular task.
type PlaneMember struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	FirstName   string `json:"first_name,omitempty"`
	LastName    string `json:"last_name,omitempty"`
}

func (plane *PlaneClient) ProjectMembers(ctx context.Context) ([]PlaneMember, error) {
	var members []PlaneMember
	err := plane.do(ctx, http.MethodGet, plane.projectPath("members"), nil, nil, &members)
	return members, err
}
