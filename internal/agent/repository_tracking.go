package agent

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// These are agent reports, not proof of verification, merge or human acceptance.
type RepositoryReport struct {
	Repo             string `json:"repo"`
	Role             string `json:"role"`
	Branch           string `json:"branch,omitempty"`
	BaseSHA          string `json:"baseSha,omitempty"`
	PRURL            string `json:"prUrl,omitempty"`
	ValidationStatus string `json:"validationStatus"`
}

var trackingBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
var trackingSHA = regexp.MustCompile(`^[a-fA-F0-9]{7,64}$`)

func trackingURL(raw string) string {
	if len(raw) > 1024 {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return u.String()
}

func ParseRepositoryReports(raw any) []RepositoryReport {
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) > 64<<10 {
		return nil
	}
	var reports []RepositoryReport
	if json.Unmarshal(encoded, &reports) != nil || len(reports) > 32 {
		return nil
	}
	var result []RepositoryReport
	for _, report := range reports {
		report.Repo = trackingURL(report.Repo)
		if report.Repo == "" || (report.Role != "modify" && report.Role != "consult" && report.Role != "candidate") {
			continue
		}
		report.PRURL = trackingURL(report.PRURL)
		if !trackingBranch.MatchString(report.Branch) {
			report.Branch = ""
		}
		if !trackingSHA.MatchString(report.BaseSHA) {
			report.BaseSHA = ""
		}
		switch report.ValidationStatus {
		case "passed", "failed", "not_run", "blocked":
		default:
			report.ValidationStatus = "unknown"
		}
		result = append(result, report)
	}
	return result
}

func parseRepositoryReports(raw any) []RepositoryReport { return ParseRepositoryReports(raw) }
