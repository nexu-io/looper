package sweeper

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

const proposerKindAgentV1 = "agent_v1"

//go:embed proposal_schema_v2.json
var proposalSchemaV2 string

type AgentExecutor interface {
	Start(context.Context, AgentRunInput) (AgentExecution, error)
}

type AgentExecution interface {
	Wait(context.Context) (AgentResult, error)
	Kill(string) error
}

type AgentRunInput struct {
	ExecutionID      string
	ProjectID        string
	LoopID           string
	RunID            string
	Prompt           string
	WorkingDirectory string
	Timeout          time.Duration
	HeartbeatTimeout time.Duration
	Metadata         map[string]any
	IdempotencyKey   string
}

type AgentResult struct {
	Status                       string
	Summary                      string
	Stdout                       string
	Stderr                       string
	ParseStatus                  string
	TimeoutType                  string
	ConfiguredIdleTimeoutSeconds int64
	ConfiguredMaxRuntimeSeconds  int64
	ElapsedRuntimeSeconds        int64
	LastProgressAt               string
}

type normalizedProposal struct {
	SchemaVersion int              `json:"schemaVersion"`
	Decision      string           `json:"decision"`
	Category      string           `json:"category"`
	Confidence    int              `json:"confidenceScore"`
	Summary       string           `json:"summary"`
	Rationale     string           `json:"rationale"`
	Evidence      []map[string]any `json:"evidence,omitempty"`
	MarkerUUID    string           `json:"markerUUID,omitempty"`
}

type persistedRawAgentResult struct {
	ExecutionID                  string `json:"executionId,omitempty"`
	Prompt                       string `json:"prompt,omitempty"`
	Status                       string `json:"status,omitempty"`
	Summary                      string `json:"summary,omitempty"`
	Stdout                       string `json:"stdout,omitempty"`
	Stderr                       string `json:"stderr,omitempty"`
	ParseStatus                  string `json:"parseStatus,omitempty"`
	TimeoutType                  string `json:"timeoutType,omitempty"`
	ConfiguredIdleTimeoutSeconds int64  `json:"configuredIdleTimeoutSeconds,omitempty"`
	ConfiguredMaxRuntimeSeconds  int64  `json:"configuredMaxRuntimeSeconds,omitempty"`
	ElapsedRuntimeSeconds        int64  `json:"elapsedRuntimeSeconds,omitempty"`
	LastProgressAt               string `json:"lastProgressAt,omitempty"`
}

func buildProposalPrompt(bundle FactBundle, phase, heuristicCategory, heuristicRationale string, roleCfg config.SweeperRoleConfig, runtime string, model *string) (string, error) {
	bundleJSON, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal sweeper fact bundle for prompt: %w", err)
	}
	var out strings.Builder
	out.WriteString("Return exactly one JSON object matching the sweeper proposal schema. Do not wrap it in markdown.\n\n")
	out.WriteString("Schema:\n")
	out.WriteString(proposalSchemaV2)
	out.WriteString("\n\n")
	out.WriteString(fmt.Sprintf("Phase: %s\n", strings.TrimSpace(phase)))
	if runtime = strings.TrimSpace(runtime); runtime != "" {
		out.WriteString(fmt.Sprintf("Agent runtime: %s\n", runtime))
	}
	if model != nil && strings.TrimSpace(*model) != "" {
		out.WriteString(fmt.Sprintf("Agent model: %s\n", strings.TrimSpace(*model)))
	}
	out.WriteString("Filter mode: deterministic\n")
	out.WriteString(fmt.Sprintf("Heuristic prefilter category: %s\n", strings.TrimSpace(heuristicCategory)))
	out.WriteString(fmt.Sprintf("Heuristic prefilter rationale: %s\n", strings.TrimSpace(heuristicRationale)))
	if instructions := strings.TrimSpace(roleCfg.Instructions); instructions != "" {
		out.WriteString("\nSweeper instructions:\n")
		out.WriteString(instructions)
		out.WriteString("\n")
	}
	out.WriteString(`
Rules:
- Use decision=warn or decision=close conservatively.
- Use decision=no_action only with category=none.
- Do not use decision=quarantine; route_security remains deterministic prefilter-only for now.
- Prefer linked PR and timeline evidence for already_fixed and superseded when available.
- Include evidence[] references with bundlePath pointers when using non-trivial evidence.
- confidenceScore must be 0..100.
- summary and rationale must be concise but specific.
- markerUUID is optional and should only be set for warn decisions.

Fact bundle:
`)
	out.Write(bundleJSON)
	out.WriteString("\n")
	return out.String(), nil
}

func parseNormalizedProposal(raw string) (normalizedProposal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return normalizedProposal{}, fmt.Errorf("agent returned empty output")
	}
	start := strings.Index(raw, "{")
	if start < 0 {
		return normalizedProposal{}, fmt.Errorf("agent output did not contain a JSON object")
	}
	var proposal normalizedProposal
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw[start:])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return normalizedProposal{}, fmt.Errorf("parse agent proposal json: %w", err)
	}
	if decoder.More() {
		return normalizedProposal{}, fmt.Errorf("parse agent proposal json: trailing content after object")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return normalizedProposal{}, fmt.Errorf("parse agent proposal json: trailing content after object")
	}
	return proposal, nil
}

func validateNormalizedProposal(proposal normalizedProposal, phase string) error {
	if proposal.SchemaVersion != 2 {
		return fmt.Errorf("schemaVersion must be 2")
	}
	if proposal.Confidence < 0 || proposal.Confidence > 100 {
		return fmt.Errorf("confidenceScore must be between 0 and 100")
	}
	if strings.TrimSpace(proposal.Summary) == "" {
		return fmt.Errorf("summary is required")
	}
	if strings.TrimSpace(proposal.Rationale) == "" {
		return fmt.Errorf("rationale is required")
	}
	decision := strings.TrimSpace(proposal.Decision)
	category := strings.TrimSpace(proposal.Category)
	switch decision {
	case "no_action":
		if category != categoryNone {
			return fmt.Errorf("decision no_action requires category %q", categoryNone)
		}
	case "warn":
		if phase != "warn" {
			return fmt.Errorf("decision warn requires phase warn")
		}
		if category != categoryStale && category != categoryAbandonedPR && category != categoryAlreadyFixed && category != categorySuperseded {
			return fmt.Errorf("decision warn requires supported warn category")
		}
	case "close":
		if phase != "close" {
			return fmt.Errorf("decision close requires phase close")
		}
		if category != categoryStale && category != categoryAbandonedPR && category != categoryAlreadyFixed && category != categorySuperseded {
			return fmt.Errorf("decision close requires supported close category")
		}
	case "quarantine":
		return fmt.Errorf("decision quarantine is not accepted from proposer output; route_security remains prefilter-only")
	case "cancel":
		if phase != "reconcile" {
			return fmt.Errorf("decision cancel requires phase reconcile")
		}
	case "stale_proposal":
		return fmt.Errorf("decision stale_proposal is not accepted from proposer output")
	default:
		return fmt.Errorf("unsupported decision %q", decision)
	}
	if decision == "warn" && strings.TrimSpace(proposal.MarkerUUID) == "" {
		return fmt.Errorf("markerUUID is required for warn decisions")
	}
	if decision != "warn" && strings.TrimSpace(proposal.MarkerUUID) != "" {
		return fmt.Errorf("markerUUID is only valid for warn decisions")
	}
	return nil
}

func marshalRawAgentResult(executionID, prompt string, result AgentResult) *string {
	encoded, err := json.Marshal(persistedRawAgentResult{
		ExecutionID:                  executionID,
		Prompt:                       prompt,
		Status:                       result.Status,
		Summary:                      result.Summary,
		Stdout:                       result.Stdout,
		Stderr:                       result.Stderr,
		ParseStatus:                  result.ParseStatus,
		TimeoutType:                  result.TimeoutType,
		ConfiguredIdleTimeoutSeconds: result.ConfiguredIdleTimeoutSeconds,
		ConfiguredMaxRuntimeSeconds:  result.ConfiguredMaxRuntimeSeconds,
		ElapsedRuntimeSeconds:        result.ElapsedRuntimeSeconds,
		LastProgressAt:               result.LastProgressAt,
	})
	if err != nil {
		return nil
	}
	value := string(encoded)
	return &value
}

func (r *Runner) persistAgentProposal(ctx context.Context, projectID string, target liveTarget, payload sweeperPayload, caseRecord *storage.SweeperCaseRecord, roleCfg config.SweeperRoleConfig, phase, heuristicCategory, heuristicRationale, executionID, prompt string, executionResult AgentResult, proposal normalizedProposal) (*storage.SweeperProposalRecord, string, error) {
	factBundle := r.buildFactBundle(target, caseRecord, roleCfg)
	factBundleJSON, err := json.Marshal(factBundle)
	if err != nil {
		return nil, "", fmt.Errorf("marshal sweeper fact bundle: %w", err)
	}
	fingerprintJSON, err := BuildFingerprint(factBundle)
	if err != nil {
		return nil, "", fmt.Errorf("build sweeper fingerprint: %w", err)
	}
	proposalJSON, err := json.Marshal(proposal)
	if err != nil {
		return nil, "", fmt.Errorf("marshal normalized proposal: %w", err)
	}
	validationStatus := "passed"
	record := storage.SweeperProposalRecord{
		ID:               eventlog.NewEventID("sweeper_proposal"),
		CaseID:           caseRecord.ID,
		ProjectID:        projectID,
		Repo:             caseRecord.Repo,
		TargetType:       targetTypeFromBool(target.IsPR),
		TargetNumber:     target.Number,
		SchemaVersion:    int64(proposal.SchemaVersion),
		ProposerKind:     proposerKindAgentV1,
		FactBundleJSON:   string(factBundleJSON),
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
		return nil, "", err
	}
	if err := r.writeDurableReport(projectID, target, payload, caseRecord, &record, roleCfg); err != nil {
		return nil, "", err
	}
	_ = phase
	_ = heuristicCategory
	_ = heuristicRationale
	return &record, fingerprintJSON, nil
}

func (r *Runner) persistInvalidAgentProposal(ctx context.Context, projectID string, target liveTarget, caseRecord *storage.SweeperCaseRecord, roleCfg config.SweeperRoleConfig, executionID, prompt string, executionResult AgentResult, validationErr error) error {
	if caseRecord == nil || r.repos == nil || r.repos.SweeperProposals == nil {
		return nil
	}
	factBundle := r.buildFactBundle(target, caseRecord, roleCfg)
	factBundleJSON, err := json.Marshal(factBundle)
	if err != nil {
		return err
	}
	fingerprintJSON, err := BuildFingerprint(factBundle)
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
		TargetType:       targetTypeFromBool(target.IsPR),
		TargetNumber:     target.Number,
		SchemaVersion:    2,
		ProposerKind:     proposerKindAgentV1,
		FactBundleJSON:   string(factBundleJSON),
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
	return r.writeDurableReport(projectID, target, sweeperPayload{Repo: caseRecord.Repo, TargetType: targetTypeFromBool(target.IsPR), TargetNumber: target.Number, Category: categoryNone, Summary: validationErr.Error()}, caseRecord, &record, roleCfg)
}
