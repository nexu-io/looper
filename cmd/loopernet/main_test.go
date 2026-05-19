package main

import "testing"

func TestRunRejectsMissingRequiredEnv(t *testing.T) {
	if exitCode := run([]string{"LOOPERNET_ADMIN_TOKEN=admin"}); exitCode != 1 {
		t.Fatalf("run() exit code = %d, want 1", exitCode)
	}
}
