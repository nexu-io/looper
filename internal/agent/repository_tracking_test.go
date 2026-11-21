package agent

import "testing"

func TestRepositoryReportsRejectCredentialsAndPreserveUnknownVerification(t *testing.T) {
	result := parseCompletion(`__LOOPER_RESULT__={"summary":"done","repositories":[{"repo":"https://github.com/acme/client","role":"modify","branch":"codex/test","prUrl":"https://github.com/acme/client/pull/1","validationStatus":"passed"},{"repo":"https://code.example/core/cloud","role":"consult","validationStatus":"not_run"},{"repo":"https://user:secret@code.example/core/private","role":"modify"},{"repo":"https://code.example/core/private?token=secret","role":"modify"},{"repo":"https://code.example/core/unknown","role":"candidate","validationStatus":"made_up"}]}`, "")
	if len(result.RepositoryReports) != 3 || result.RepositoryReports[0].Branch != "codex/test" || result.RepositoryReports[1].ValidationStatus != "not_run" || result.RepositoryReports[2].ValidationStatus != "unknown" {
		t.Fatalf("bad repository reports: %+v", result.RepositoryReports)
	}
}
