package webhookforward

import (
	"testing"

	"github.com/nexu-io/looper/internal/config"
)

// Webhook PR events must route to LaneShepherd so a shepherding worker loop is
// woken event-driven (not just the 60s poll). CI check bursts coalesce by PR
// (the forwarder's existing work-item dedup), so many failing checks fold to one
// shepherd wake.
func TestRouteDeliveryAddsShepherdLane(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    bool
	}{
		{"review", "pull_request_review", `{"action":"submitted","repository":{"full_name":"o/r"},"pull_request":{"number":5}}`, true},
		{"synchronize", "pull_request", `{"action":"synchronize","repository":{"full_name":"o/r"},"pull_request":{"number":5}}`, true},
		{"closed", "pull_request", `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":5}}`, true},
		{"check_run failing", "check_run", `{"action":"completed","repository":{"full_name":"o/r"},"check_run":{"conclusion":"failure","pull_requests":[{"number":5}]}}`, true},
	}
	for _, c := range cases {
		routed, ok, err := routeDelivery(c.event, []byte(c.payload))
		if err != nil || !ok {
			t.Fatalf("%s: routeDelivery ok=%v err=%v", c.name, ok, err)
		}
		_, has := routed.lanes[LaneShepherd]
		if has != c.want {
			t.Fatalf("%s: LaneShepherd present=%v want=%v (lanes=%v)", c.name, has, c.want, routed.lanes)
		}
	}
}

// enabledLanesForProject always lets the shepherd lane through (it is gated
// per-loop by the durable marker, not by a discovery role config).
func TestShepherdLaneAlwaysEnabled(t *testing.T) {
	out := enabledLanesForProject(testEmptyConfig(), "proj", map[Lane]struct{}{LaneShepherd: {}})
	if _, ok := out[LaneShepherd]; !ok {
		t.Fatal("shepherd lane must pass enabledLanesForProject regardless of role config")
	}
}

func testEmptyConfig() config.Config { return config.Config{} }
