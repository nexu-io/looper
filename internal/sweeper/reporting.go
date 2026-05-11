package sweeper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func (r *Runner) writeDurableReport(projectID string, target liveTarget, payload sweeperPayload, caseRecord *storage.SweeperCaseRecord, proposal *storage.SweeperProposalRecord, roleCfg config.SweeperRoleConfig) error {
	root := strings.TrimSpace(roleCfg.Reporting.DurableReportsDir)
	if root == "" {
		return nil
	}
	repo := strings.TrimSpace(payload.Repo)
	if repo == "" && caseRecord != nil {
		repo = strings.TrimSpace(caseRecord.Repo)
	}
	if repo == "" {
		return nil
	}
	targetType := strings.TrimSpace(payload.TargetType)
	if targetType == "" {
		targetType = targetTypeFromBool(target.IsPR)
	}
	number := payload.TargetNumber
	if number <= 0 {
		number = target.Number
	}
	if number <= 0 {
		return nil
	}
	path := filepath.Join(root, projectID, filepath.FromSlash(repo), fmt.Sprintf("%s-%d.md", strings.ReplaceAll(targetType, "_", "-"), number))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create sweeper durable report dir: %w", err)
	}
	content := buildDurableReportMarkdown(r.nowISO(), projectID, repo, targetType, number, target, payload, caseRecord, proposal)
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write sweeper durable report: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("finalize sweeper durable report: %w", err)
	}
	return nil
}

func buildDurableReportMarkdown(generatedAt, projectID, repo, targetType string, number int64, target liveTarget, payload sweeperPayload, caseRecord *storage.SweeperCaseRecord, proposal *storage.SweeperProposalRecord) string {
	title := strings.TrimSpace(target.Title)
	if title == "" {
		title = "(unknown title)"
	}
	body, _ := TruncateFactBody(target.Body)
	var out strings.Builder
	out.WriteString("# Sweeper Report\n\n")
	out.WriteString(fmt.Sprintf("- Generated at: %s\n", strings.TrimSpace(generatedAt)))
	out.WriteString(fmt.Sprintf("- Project: %s\n", strings.TrimSpace(projectID)))
	out.WriteString(fmt.Sprintf("- Repo: %s\n", strings.TrimSpace(repo)))
	out.WriteString(fmt.Sprintf("- Target: %s #%d\n", strings.TrimSpace(targetType), number))
	if caseRecord != nil {
		out.WriteString(fmt.Sprintf("- Case ID: %s\n", strings.TrimSpace(caseRecord.ID)))
		out.WriteString(fmt.Sprintf("- Case status: %s\n", strings.TrimSpace(caseRecord.Status)))
		out.WriteString(fmt.Sprintf("- Case phase: %s\n", strings.TrimSpace(caseRecord.CurrentPhase)))
	}
	out.WriteString(fmt.Sprintf("- Outcome: %s\n", strings.TrimSpace(payload.Outcome)))
	out.WriteString(fmt.Sprintf("- Category: %s\n", strings.TrimSpace(payload.Category)))
	if payload.Confidence > 0 {
		out.WriteString(fmt.Sprintf("- Confidence: %d\n", payload.Confidence))
	}
	if proposal != nil {
		out.WriteString(fmt.Sprintf("- Proposal ID: %s\n", strings.TrimSpace(proposal.ID)))
		out.WriteString(fmt.Sprintf("- Proposal kind: %s\n", strings.TrimSpace(proposal.ProposerKind)))
		out.WriteString(fmt.Sprintf("- Decision: %s\n", strings.TrimSpace(proposal.Decision)))
		if proposal.ValidationStatus != nil {
			out.WriteString(fmt.Sprintf("- Validation status: %s\n", strings.TrimSpace(*proposal.ValidationStatus)))
		}
		if proposal.ApplyStatus != nil {
			out.WriteString(fmt.Sprintf("- Apply status: %s\n", strings.TrimSpace(*proposal.ApplyStatus)))
		}
	}
	out.WriteString("\n## Target\n\n")
	out.WriteString(fmt.Sprintf("**Title:** %s\n\n", title))
	if strings.TrimSpace(target.State) != "" {
		out.WriteString(fmt.Sprintf("- State: %s\n", strings.TrimSpace(target.State)))
	}
	if strings.TrimSpace(target.Author) != "" {
		out.WriteString(fmt.Sprintf("- Author: %s\n", strings.TrimSpace(target.Author)))
	}
	if strings.TrimSpace(target.UpdatedAt) != "" {
		out.WriteString(fmt.Sprintf("- Updated at: %s\n", strings.TrimSpace(target.UpdatedAt)))
	}
	if len(target.Labels) > 0 {
		out.WriteString(fmt.Sprintf("- Labels: %s\n", strings.Join(target.Labels, ", ")))
	}
	out.WriteString("\n## Summary\n\n")
	if strings.TrimSpace(payload.Summary) != "" {
		out.WriteString(strings.TrimSpace(payload.Summary))
		out.WriteString("\n\n")
	}
	if strings.TrimSpace(payload.Rationale) != "" {
		out.WriteString(strings.TrimSpace(payload.Rationale))
		out.WriteString("\n\n")
	}
	if proposal != nil {
		if proposal.Summary != nil && strings.TrimSpace(*proposal.Summary) != "" {
			out.WriteString(fmt.Sprintf("Proposal summary: %s\n\n", strings.TrimSpace(*proposal.Summary)))
		}
		if proposal.Rationale != nil && strings.TrimSpace(*proposal.Rationale) != "" {
			out.WriteString(fmt.Sprintf("Proposal rationale: %s\n\n", strings.TrimSpace(*proposal.Rationale)))
		}
		if proposal.ValidationError != nil && strings.TrimSpace(*proposal.ValidationError) != "" {
			out.WriteString(fmt.Sprintf("Validation error: %s\n\n", strings.TrimSpace(*proposal.ValidationError)))
		}
		if proposal.ApplySummary != nil && strings.TrimSpace(*proposal.ApplySummary) != "" {
			out.WriteString(fmt.Sprintf("Apply summary: %s\n\n", strings.TrimSpace(*proposal.ApplySummary)))
		}
		if proposal.ApplyError != nil && strings.TrimSpace(*proposal.ApplyError) != "" {
			out.WriteString(fmt.Sprintf("Apply error: %s\n\n", strings.TrimSpace(*proposal.ApplyError)))
		}
	}
	if strings.TrimSpace(payload.CloseBy) != "" {
		out.WriteString(fmt.Sprintf("Close by: %s\n\n", strings.TrimSpace(payload.CloseBy)))
	}
	out.WriteString("## Body\n\n````text\n")
	out.WriteString(strings.TrimSpace(body))
	out.WriteString("\n````\n")
	return out.String()
}
