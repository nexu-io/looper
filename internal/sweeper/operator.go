package sweeper

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

type CaseQuery struct {
	ProjectID string
	Repo      string
	Phase     string
	Status    string
	Limit     int
}

type CaseInspection struct {
	Case      storage.SweeperCaseRecord
	Proposals []storage.SweeperProposalRecord
}

type RepoStats struct {
	ProjectID               string         `json:"projectId"`
	Repo                    string         `json:"repo"`
	CaseCount               int            `json:"caseCount"`
	ProposalCount           int            `json:"proposalCount"`
	ProposalsByProposerKind map[string]int `json:"proposalsByProposerKind"`
	ApplyOutcomes           map[string]int `json:"applyOutcomes"`
	CurrentPhases           map[string]int `json:"currentPhases"`
	StaleRate               float64        `json:"staleRate"`
	AgentTimeoutRate        float64        `json:"agentTimeoutRate"`
	AgentTimeouts           int            `json:"agentTimeouts"`
	AgentProposalCount      int            `json:"agentProposalCount"`
}

func (r *Runner) ListCases(ctx context.Context, query CaseQuery) ([]storage.SweeperCaseRecord, error) {
	if r.repos == nil || r.repos.SweeperCases == nil {
		return nil, fmt.Errorf("sweeper case repository is not configured")
	}
	projectID := strings.TrimSpace(query.ProjectID)
	repo := strings.TrimSpace(query.Repo)
	if projectID == "" || repo == "" {
		return nil, fmt.Errorf("project id and repo are required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	records, err := r.repos.SweeperCases.ListByProjectRepo(ctx, projectID, repo, limit)
	if err != nil {
		return nil, err
	}
	phase := strings.TrimSpace(query.Phase)
	status := strings.TrimSpace(query.Status)
	if phase == "" && status == "" {
		return records, nil
	}
	filtered := make([]storage.SweeperCaseRecord, 0, len(records))
	for _, record := range records {
		if phase != "" && strings.TrimSpace(record.CurrentPhase) != phase {
			continue
		}
		if status != "" && strings.TrimSpace(record.Status) != status {
			continue
		}
		filtered = append(filtered, record)
	}
	return filtered, nil
}

func (r *Runner) InspectCase(ctx context.Context, caseID string) (*CaseInspection, error) {
	if r.repos == nil || r.repos.SweeperCases == nil || r.repos.SweeperProposals == nil {
		return nil, fmt.Errorf("sweeper repositories are not configured")
	}
	caseID = strings.TrimSpace(caseID)
	if caseID == "" {
		return nil, fmt.Errorf("case id is required")
	}
	caseRecord, err := r.repos.SweeperCases.GetByID(ctx, caseID)
	if err != nil {
		return nil, err
	}
	if caseRecord == nil {
		return nil, fmt.Errorf("sweeper case not found: %s", caseID)
	}
	proposals, err := r.repos.SweeperProposals.ListByCaseID(ctx, caseID)
	if err != nil {
		return nil, err
	}
	return &CaseInspection{Case: *caseRecord, Proposals: proposals}, nil
}

func (r *Runner) RepoOperatorStats(ctx context.Context, projectID, repo string, limit int) (RepoStats, error) {
	if r.repos == nil || r.repos.SweeperCases == nil || r.repos.SweeperProposals == nil {
		return RepoStats{}, fmt.Errorf("sweeper repositories are not configured")
	}
	projectID = strings.TrimSpace(projectID)
	repo = strings.TrimSpace(repo)
	if projectID == "" || repo == "" {
		return RepoStats{}, fmt.Errorf("project id and repo are required")
	}
	if limit <= 0 {
		limit = 1000
	}
	cases, err := r.repos.SweeperCases.ListByProjectRepo(ctx, projectID, repo, limit)
	if err != nil {
		return RepoStats{}, err
	}
	proposals, err := r.repos.SweeperProposals.ListByProjectRepo(ctx, projectID, repo, limit)
	if err != nil {
		return RepoStats{}, err
	}
	return buildRepoStats(projectID, repo, cases, proposals), nil
}

func (r *Runner) ReplayCaseProposalDryRun(ctx context.Context, caseID string) (*storage.SweeperProposalRecord, error) {
	if r.repos == nil || r.repos.SweeperCases == nil || r.repos.SweeperProposals == nil {
		return nil, fmt.Errorf("sweeper repositories are not configured")
	}
	caseRecord, err := r.repos.SweeperCases.GetByID(ctx, strings.TrimSpace(caseID))
	if err != nil {
		return nil, err
	}
	if caseRecord == nil {
		return nil, fmt.Errorf("sweeper case not found: %s", strings.TrimSpace(caseID))
	}
	project, roleCfg, err := r.projectConfig(ctx, caseRecord.ProjectID)
	if err != nil {
		return nil, err
	}
	baseProposal, err := r.loadReplayBaseProposal(ctx, caseRecord)
	if err != nil {
		return nil, err
	}
	if baseProposal == nil {
		return nil, fmt.Errorf("no sweeper proposal found for case %s", caseRecord.ID)
	}
	bundle, err := parseFactBundle(baseProposal.FactBundleJSON)
	if err != nil {
		return nil, err
	}
	phase, err := replayPhase(caseRecord, baseProposal)
	if err != nil {
		return nil, err
	}
	target := liveTargetFromFactBundle(bundle)
	heuristicCategory, confidence, heuristicRationale := classifyTarget(target, roleCfg, r.now())
	if roleCfg.Proposer.Mode == config.SweeperProposerModeHeuristicFallback || r.agent == nil {
		return r.persistReplayHeuristicProposal(ctx, caseRecord.ProjectID, caseRecord, bundle, target, heuristicCategory, confidence, heuristicRationale)
	}
	return r.replayAgentProposal(ctx, project, caseRecord, roleCfg, phase, bundle, target, heuristicCategory, heuristicRationale)
}

func buildRepoStats(projectID, repo string, cases []storage.SweeperCaseRecord, proposals []storage.SweeperProposalRecord) RepoStats {
	stats := RepoStats{
		ProjectID:               projectID,
		Repo:                    repo,
		CaseCount:               len(cases),
		ProposalCount:           len(proposals),
		ProposalsByProposerKind: map[string]int{},
		ApplyOutcomes:           map[string]int{},
		CurrentPhases:           map[string]int{},
	}
	staleCount := 0
	agentProposalCount := 0
	agentTimeouts := 0
	for _, record := range cases {
		stats.CurrentPhases[strings.TrimSpace(record.CurrentPhase)]++
	}
	for _, record := range proposals {
		stats.ProposalsByProposerKind[strings.TrimSpace(record.ProposerKind)]++
		applyStatus := "pending"
		if record.ApplyStatus != nil && strings.TrimSpace(*record.ApplyStatus) != "" {
			applyStatus = strings.TrimSpace(*record.ApplyStatus)
		}
		stats.ApplyOutcomes[applyStatus]++
		if strings.TrimSpace(record.Category) == categoryStale {
			staleCount++
		}
		if strings.TrimSpace(record.ProposerKind) == proposerKindAgentV1 {
			agentProposalCount++
			if proposalTimedOut(record.RawResultJSON) {
				agentTimeouts++
			}
		}
	}
	stats.AgentProposalCount = agentProposalCount
	stats.AgentTimeouts = agentTimeouts
	if len(proposals) > 0 {
		stats.StaleRate = float64(staleCount) / float64(len(proposals))
	}
	if agentProposalCount > 0 {
		stats.AgentTimeoutRate = float64(agentTimeouts) / float64(agentProposalCount)
	}
	return stats
}

func proposalTimedOut(raw *string) bool {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return false
	}
	var parsed struct {
		Status      string `json:"status,omitempty"`
		TimeoutType string `json:"timeoutType,omitempty"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(*raw)), &parsed); err != nil {
		return false
	}
	return strings.TrimSpace(parsed.TimeoutType) != "" || strings.EqualFold(strings.TrimSpace(parsed.Status), "timeout")
}

func SortedCountKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (r *Runner) loadReplayBaseProposal(ctx context.Context, caseRecord *storage.SweeperCaseRecord) (*storage.SweeperProposalRecord, error) {
	if caseRecord == nil || r.repos == nil || r.repos.SweeperProposals == nil {
		return nil, nil
	}
	if caseRecord.LastProposalID != nil && strings.TrimSpace(*caseRecord.LastProposalID) != "" {
		proposal, err := r.repos.SweeperProposals.GetByID(ctx, strings.TrimSpace(*caseRecord.LastProposalID))
		if err != nil {
			return nil, err
		}
		if proposal != nil {
			return proposal, nil
		}
	}
	return r.repos.SweeperProposals.GetLatestByCaseID(ctx, caseRecord.ID)
}

func replayPhase(caseRecord *storage.SweeperCaseRecord, proposal *storage.SweeperProposalRecord) (string, error) {
	if caseRecord != nil {
		switch strings.TrimSpace(caseRecord.CurrentPhase) {
		case "warn", "close", "reconcile":
			return strings.TrimSpace(caseRecord.CurrentPhase), nil
		}
	}
	if proposal != nil {
		switch strings.TrimSpace(proposal.Decision) {
		case "warn":
			return "warn", nil
		case "close", "quarantine":
			return "close", nil
		case "cancel":
			return "reconcile", nil
		}
	}
	return "", fmt.Errorf("case is not in a replayable phase")
}

func parseFactBundle(raw string) (FactBundle, error) {
	var bundle FactBundle
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &bundle); err != nil {
		return FactBundle{}, fmt.Errorf("parse fact bundle: %w", err)
	}
	if strings.TrimSpace(bundle.Repo) == "" || bundle.Number <= 0 {
		return FactBundle{}, fmt.Errorf("fact bundle is missing repo or target number")
	}
	return bundle, nil
}

func liveTargetFromFactBundle(bundle FactBundle) liveTarget {
	return liveTarget{
		Number:            bundle.Number,
		State:             bundle.State,
		Title:             bundle.Title,
		Body:              bundle.Body,
		CreatedAt:         bundle.CreatedAt,
		UpdatedAt:         bundle.UpdatedAt,
		ClosedAt:          bundle.ClosedAt,
		Author:            bundle.Author,
		AuthorAssociation: bundle.AuthorAssociation,
		Labels:            append([]string(nil), bundle.Labels...),
		CommentCount:      bundle.CommentCount,
		HeadSHA:           bundle.HeadSHA,
		IsPR:              strings.TrimSpace(bundle.TargetType) == "pull_request",
		Draft:             bundle.IsDraft,
	}
}

func (r *Runner) replayAgentProposal(ctx context.Context, project *storage.ProjectRecord, caseRecord *storage.SweeperCaseRecord, roleCfg config.SweeperRoleConfig, phase string, bundle FactBundle, target liveTarget, heuristicCategory string, heuristicRationale string) (*storage.SweeperProposalRecord, error) {
	prompt, err := buildProposalPrompt(bundle, phase, heuristicCategory, heuristicRationale, roleCfg, r.agentRuntime, modelOrOverride(roleCfg.Proposer.Model, r.agentModel))
	if err != nil {
		return nil, err
	}
	executionID := eventlog.NewEventID("agent_execution")
	execution, err := r.agent.Start(ctx, AgentRunInput{
		ExecutionID:      executionID,
		ProjectID:        caseRecord.ProjectID,
		RunID:            executionID,
		Prompt:           prompt,
		WorkingDirectory: project.RepoPath,
		Timeout:          time.Duration(roleCfg.Proposer.TimeoutSeconds) * time.Second,
		HeartbeatTimeout: time.Duration(roleCfg.Proposer.TimeoutSeconds) * time.Second,
		Metadata: map[string]any{
			"role":              "sweeper",
			"sweeperPhase":      phase,
			"heuristicCategory": heuristicCategory,
			"repo":              caseRecord.Repo,
			"targetType":        caseRecord.TargetType,
			"targetNumber":      caseRecord.TargetNumber,
			"replay":            true,
		},
		IdempotencyKey: fmt.Sprintf("sweeper:replay:%s:%s:%d", phase, caseRecord.Repo, caseRecord.TargetNumber),
	})
	if err != nil {
		return nil, fmt.Errorf("start sweeper proposer replay agent: %w", err)
	}
	result, err := execution.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for sweeper proposer replay agent: %w", err)
	}
	if result.Status != "completed" {
		statusErr := fmt.Errorf("sweeper proposer execution status %q", result.Status)
		_ = r.persistInvalidReplayAgentProposal(ctx, caseRecord.ProjectID, caseRecord, bundle, targetTypeFromBool(target.IsPR), executionID, prompt, result, statusErr)
		return nil, statusErr
	}
	proposal, parseErr := parseNormalizedProposal(firstNonEmpty(result.Stdout, result.Summary))
	if parseErr != nil {
		_ = r.persistInvalidReplayAgentProposal(ctx, caseRecord.ProjectID, caseRecord, bundle, targetTypeFromBool(target.IsPR), executionID, prompt, result, parseErr)
		return nil, parseErr
	}
	if validateErr := validateNormalizedProposal(proposal, phase); validateErr != nil {
		_ = r.persistInvalidReplayAgentProposal(ctx, caseRecord.ProjectID, caseRecord, bundle, targetTypeFromBool(target.IsPR), executionID, prompt, result, validateErr)
		return nil, validateErr
	}
	return r.persistReplayAgentProposal(ctx, caseRecord.ProjectID, caseRecord, bundle, targetTypeFromBool(target.IsPR), executionID, prompt, result, proposal)
}

func (r *Runner) persistReplayHeuristicProposal(ctx context.Context, projectID string, caseRecord *storage.SweeperCaseRecord, bundle FactBundle, target liveTarget, category string, confidence int, rationale string) (*storage.SweeperProposalRecord, error) {
	decision := categoryDecisionForPhase(replayDefaultPhase(caseRecord), category)
	markerUUID := ""
	if decision == "warn" {
		markerUUID = NewMarkerUUID()
	}
	proposalBody, err := json.Marshal(map[string]any{
		"schemaVersion":   2,
		"decision":        decision,
		"category":        category,
		"confidenceScore": confidence,
		"summary":         "sweeper replay heuristic proposal",
		"rationale":       rationale,
		"markerUUID":      markerUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal replay heuristic proposal: %w", err)
	}
	factBundleJSON, fingerprintJSON, err := marshalReplayBundle(bundle)
	if err != nil {
		return nil, err
	}
	validationStatus := "passed"
	summary := "sweeper replay heuristic proposal"
	record := storage.SweeperProposalRecord{
		ID:               eventlog.NewEventID("sweeper_proposal"),
		CaseID:           caseRecord.ID,
		ProjectID:        projectID,
		Repo:             caseRecord.Repo,
		TargetType:       targetTypeFromBool(target.IsPR),
		TargetNumber:     target.Number,
		SchemaVersion:    2,
		ProposerKind:     "heuristic_v1",
		FactBundleJSON:   factBundleJSON,
		FingerprintJSON:  fingerprintJSON,
		ProposalJSON:     string(proposalBody),
		Decision:         decision,
		Category:         category,
		ConfidenceScore:  int64(confidence),
		Summary:          &summary,
		Rationale:        optionalString(rationale),
		MarkerUUID:       optionalString(markerUUID),
		ValidationStatus: &validationStatus,
		CreatedAt:        r.nowISO(),
	}
	if err := r.repos.SweeperProposals.Insert(ctx, record); err != nil {
		return nil, err
	}
	if project, roleCfg, err := r.projectConfig(ctx, projectID); err == nil && project != nil {
		if writeErr := r.writeDurableReport(projectID, target, sweeperPayload{Repo: caseRecord.Repo, TargetType: targetTypeFromBool(target.IsPR), TargetNumber: target.Number, Category: category, Confidence: confidence, Summary: summary, Rationale: rationale}, caseRecord, &record, roleCfg); writeErr != nil {
			return nil, writeErr
		}
	}
	return &record, nil
}

func replayDefaultPhase(caseRecord *storage.SweeperCaseRecord) string {
	if caseRecord == nil {
		return "warn"
	}
	switch strings.TrimSpace(caseRecord.CurrentPhase) {
	case "warn", "close", "reconcile":
		return strings.TrimSpace(caseRecord.CurrentPhase)
	default:
		return "warn"
	}
}

func marshalReplayBundle(bundle FactBundle) (string, string, error) {
	factBundleJSON, err := json.Marshal(bundle)
	if err != nil {
		return "", "", fmt.Errorf("marshal sweeper fact bundle: %w", err)
	}
	fingerprintJSON, err := BuildFingerprint(bundle)
	if err != nil {
		return "", "", fmt.Errorf("build sweeper fingerprint: %w", err)
	}
	return string(factBundleJSON), fingerprintJSON, nil
}

func (r *Runner) persistReplayAgentProposal(ctx context.Context, projectID string, caseRecord *storage.SweeperCaseRecord, bundle FactBundle, targetType string, executionID, prompt string, executionResult AgentResult, proposal normalizedProposal) (*storage.SweeperProposalRecord, error) {
	factBundleJSON, fingerprintJSON, err := marshalReplayBundle(bundle)
	if err != nil {
		return nil, err
	}
	proposalJSON, err := json.Marshal(proposal)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized replay proposal: %w", err)
	}
	validationStatus := "passed"
	record := storage.SweeperProposalRecord{
		ID:               eventlog.NewEventID("sweeper_proposal"),
		CaseID:           caseRecord.ID,
		ProjectID:        projectID,
		Repo:             caseRecord.Repo,
		TargetType:       targetType,
		TargetNumber:     caseRecord.TargetNumber,
		SchemaVersion:    int64(proposal.SchemaVersion),
		ProposerKind:     proposerKindAgentV1,
		FactBundleJSON:   factBundleJSON,
		FingerprintJSON:  fingerprintJSON,
		ProposalJSON:     string(proposalJSON),
		RawResultJSON:    marshalRawAgentResult(executionID, prompt, executionResult),
		Decision:         proposal.Decision,
		Category:         proposal.Category,
		ConfidenceScore:  int64(proposal.Confidence),
		Summary:          optionalString(proposal.Summary),
		Rationale:        optionalString(proposal.Rationale),
		MarkerUUID:       optionalString(proposal.MarkerUUID),
		ValidationStatus: &validationStatus,
		CreatedAt:        r.nowISO(),
	}
	if err := r.repos.SweeperProposals.Insert(ctx, record); err != nil {
		return nil, err
	}
	if project, roleCfg, err := r.projectConfig(ctx, projectID); err == nil && project != nil {
		if writeErr := r.writeDurableReport(projectID, liveTarget{Number: caseRecord.TargetNumber, Title: bundle.Title, Body: bundle.Body, State: bundle.State, UpdatedAt: bundle.UpdatedAt, Author: bundle.Author, IsPR: targetType == "pull_request"}, sweeperPayload{Repo: caseRecord.Repo, TargetType: targetType, TargetNumber: caseRecord.TargetNumber, Category: proposal.Category, Confidence: proposal.Confidence, Summary: proposal.Summary, Rationale: proposal.Rationale}, caseRecord, &record, roleCfg); writeErr != nil {
			return nil, writeErr
		}
	}
	return &record, nil
}

func (r *Runner) persistInvalidReplayAgentProposal(ctx context.Context, projectID string, caseRecord *storage.SweeperCaseRecord, bundle FactBundle, targetType, executionID, prompt string, executionResult AgentResult, validationErr error) error {
	factBundleJSON, fingerprintJSON, err := marshalReplayBundle(bundle)
	if err != nil {
		return err
	}
	validationStatus := "failed"
	validationError := validationErr.Error()
	record := storage.SweeperProposalRecord{
		ID:               eventlog.NewEventID("sweeper_proposal"),
		CaseID:           caseRecord.ID,
		ProjectID:        projectID,
		Repo:             caseRecord.Repo,
		TargetType:       targetType,
		TargetNumber:     caseRecord.TargetNumber,
		SchemaVersion:    2,
		ProposerKind:     proposerKindAgentV1,
		FactBundleJSON:   factBundleJSON,
		FingerprintJSON:  fingerprintJSON,
		ProposalJSON:     `{}`,
		RawResultJSON:    marshalRawAgentResult(executionID, prompt, executionResult),
		Decision:         "no_action",
		Category:         categoryNone,
		ConfidenceScore:  0,
		ValidationStatus: &validationStatus,
		ValidationError:  &validationError,
		CreatedAt:        r.nowISO(),
	}
	if err := r.repos.SweeperProposals.Insert(ctx, record); err != nil {
		return err
	}
	if project, roleCfg, err := r.projectConfig(ctx, projectID); err == nil && project != nil {
		if writeErr := r.writeDurableReport(projectID, liveTarget{Number: caseRecord.TargetNumber, Title: bundle.Title, Body: bundle.Body, State: bundle.State, UpdatedAt: bundle.UpdatedAt, Author: bundle.Author, IsPR: targetType == "pull_request"}, sweeperPayload{Repo: caseRecord.Repo, TargetType: targetType, TargetNumber: caseRecord.TargetNumber, Category: categoryNone, Summary: validationErr.Error()}, caseRecord, &record, roleCfg); writeErr != nil {
			return writeErr
		}
	}
	return nil
}
