package runtime

import (
	"context"
	"os/exec"
	"testing"

	"github.com/nexu-io/looper/internal/processcontainment"
	"github.com/nexu-io/looper/internal/storage"
)

func TestClassifyStartupProbeEvidenceNeverConfirmedDeadFromPID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pid      int
		matches  bool
		running  bool
		probeErr error
		want     ContainmentClass
		reason   string
	}{
		{name: "pid absent", pid: 0, want: ContainmentUncertain, reason: "pid_absent"},
		{name: "pid not running", pid: 42, matches: false, running: false, want: ContainmentUncertain, reason: "pid_not_running_not_confirmed_dead"},
		{name: "identity mismatch", pid: 42, matches: false, running: true, want: ContainmentUncertain, reason: "process_identity_mismatch"},
		{name: "observed live", pid: 42, matches: true, running: true, want: ContainmentObservedLive, reason: "process_identity_matched"},
		{name: "probe error", pid: 42, probeErr: context.DeadlineExceeded, want: ContainmentUncertain, reason: "process_probe_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStartupProbeEvidence(tc.pid, tc.matches, tc.running, tc.probeErr)
			if got.Class != tc.want || got.Reason != tc.reason {
				t.Fatalf("classify = %#v, want class=%s reason=%s", got, tc.want, tc.reason)
			}
			if classificationAllowsTerminalOrRequeue(got.Class) {
				t.Fatalf("probe class %s must not allow terminal/requeue", got.Class)
			}
			if got.Class != ContainmentObservedLive && !classificationRequiresQuarantine(got.Class) {
				t.Fatalf("non-live class %s must require quarantine", got.Class)
			}
		})
	}
}

func TestClassifyConfirmedDeadOnlyFromDurableTerminalOrCurrentHandle(t *testing.T) {
	t.Parallel()

	terminal := storage.AgentExecutionRecord{ID: "e1", Status: "killed", PID: int64Ptr(99)}
	class, ok := classifyFromDurableStatusAndHandle(terminal, nil)
	if !ok || class.Class != ContainmentConfirmedDead || class.Reason != "durable_terminal_finalization" {
		t.Fatalf("terminal classification = %#v ok=%v", class, ok)
	}
	if !classificationAllowsTerminalOrRequeue(class.Class) {
		t.Fatal("confirmed_dead must allow terminal authority path")
	}

	active := storage.AgentExecutionRecord{ID: "e2", Status: "running", PID: int64Ptr(99)}
	if _, ok := classifyFromDurableStatusAndHandle(active, nil); ok {
		t.Fatal("active status without handle must not be confirmed_dead")
	}

	// Current-daemon handle that completed confirmed drain authorizes confirmed_dead.
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true binary not available")
	}
	cmd := exec.Command(truePath)
	processcontainment.Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	handle, err := processcontainment.Bind(cmd, processcontainment.Options{})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := handle.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if err := handle.Drain(context.Background()); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if !handle.ConfirmedDead() {
		t.Fatal("handle not ConfirmedDead after Drain")
	}
	class, ok = classifyFromDurableStatusAndHandle(active, handle)
	if !ok || class.Class != ContainmentConfirmedDead || class.Reason != "current_daemon_confirmed_drain" {
		t.Fatalf("drained handle classification = %#v ok=%v", class, ok)
	}
}
