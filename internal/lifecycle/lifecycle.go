package lifecycle

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/version"
)

const (
	PolicyAgentManagedWithFallback = "agent_managed_with_fallback"
	PolicyVersion                  = 1

	ActionSourceNone     = "none"
	ActionSourceAgent    = "agent"
	ActionSourceFallback = "fallback"
)

type Policy struct {
	Name            string   `json:"name"`
	Version         int      `json:"version"`
	ExpectCommit    bool     `json:"expectCommit"`
	ExpectPush      bool     `json:"expectPush"`
	ExpectPR        bool     `json:"expectPr"`
	AllowFallback   bool     `json:"allowFallback"`
	EligibleRunners []string `json:"eligibleRunners"`
}

type Actions struct {
	Commit string `json:"commit,omitempty"`
	Push   string `json:"push,omitempty"`
	PR     string `json:"pr,omitempty"`
}

type State struct {
	Policy          string   `json:"policy,omitempty"`
	PolicyVersion   int      `json:"policy_version,omitempty"`
	Branch          string   `json:"branch,omitempty"`
	BaseBranch      string   `json:"base_branch,omitempty"`
	CommitSHAs      []string `json:"commit_shas,omitempty"`
	Pushed          bool     `json:"pushed,omitempty"`
	PRNumber        int64    `json:"pr_number,omitempty"`
	PRURL           string   `json:"pr_url,omitempty"`
	PRAdopted       bool     `json:"pr_adopted,omitempty"`
	Actions         Actions  `json:"actions,omitempty"`
	ReconciledAt    string   `json:"reconciled_at,omitempty"`
	ReconciledBy    string   `json:"reconciled_by,omitempty"`
	LastError       string   `json:"last_reconciliation_error,omitempty"`
	AgentIngestedAt string   `json:"agent_ingested_at,omitempty"`
}

func (s *State) UnmarshalJSON(data []byte) error {
	type stateAlias State
	var raw struct {
		stateAlias
		PolicyVersionCamel   int      `json:"policyVersion,omitempty"`
		BaseBranchCamel      string   `json:"baseBranch,omitempty"`
		CommitSHAsCamel      []string `json:"commitShas,omitempty"`
		PRNumberCamel        int64    `json:"prNumber,omitempty"`
		PRURLCamel           string   `json:"prUrl,omitempty"`
		PRAdoptedCamel       bool     `json:"prAdopted,omitempty"`
		ReconciledAtCamel    string   `json:"reconciledAt,omitempty"`
		ReconciledByCamel    string   `json:"reconciledBy,omitempty"`
		LastErrorCamel       string   `json:"lastReconciliationError,omitempty"`
		AgentIngestedAtCamel string   `json:"agentIngestedAt,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = State(raw.stateAlias)
	if s.PolicyVersion == 0 {
		s.PolicyVersion = raw.PolicyVersionCamel
	}
	if s.BaseBranch == "" {
		s.BaseBranch = raw.BaseBranchCamel
	}
	if len(s.CommitSHAs) == 0 {
		s.CommitSHAs = raw.CommitSHAsCamel
	}
	if s.PRNumber == 0 {
		s.PRNumber = raw.PRNumberCamel
	}
	if s.PRURL == "" {
		s.PRURL = raw.PRURLCamel
	}
	if !s.PRAdopted {
		s.PRAdopted = raw.PRAdoptedCamel
	}
	if s.ReconciledAt == "" {
		s.ReconciledAt = raw.ReconciledAtCamel
	}
	if s.ReconciledBy == "" {
		s.ReconciledBy = raw.ReconciledByCamel
	}
	if s.LastError == "" {
		s.LastError = raw.LastErrorCamel
	}
	if s.AgentIngestedAt == "" {
		s.AgentIngestedAt = raw.AgentIngestedAtCamel
	}
	return nil
}

func AgentManagedWithFallbackPolicy(runner string, expectPR bool) Policy {
	return Policy{Name: PolicyAgentManagedWithFallback, Version: PolicyVersion, ExpectCommit: true, ExpectPush: true, ExpectPR: expectPR, AllowFallback: true, EligibleRunners: []string{"worker", "fixer", "planner"}}
}

func NewState(policy Policy, branch, baseBranch string) *State {
	return &State{Policy: policy.Name, PolicyVersion: policy.Version, Branch: strings.TrimSpace(branch), BaseBranch: strings.TrimSpace(baseBranch), Actions: Actions{Commit: ActionSourceNone, Push: ActionSourceNone, PR: ActionSourceNone}}
}

func FromMap(value any) (*State, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	state.Normalize()
	return &state, nil
}

func (s *State) Normalize() {
	if s == nil {
		return
	}
	if s.Policy == "" {
		s.Policy = PolicyAgentManagedWithFallback
	}
	if s.PolicyVersion == 0 {
		s.PolicyVersion = PolicyVersion
	}
	s.Branch = strings.TrimSpace(s.Branch)
	s.BaseBranch = strings.TrimSpace(s.BaseBranch)
	s.CommitSHAs = compact(s.CommitSHAs)
	if s.Actions.Commit == "" {
		s.Actions.Commit = actionSourceFromBool(len(s.CommitSHAs) > 0)
	}
	if s.Actions.Push == "" {
		s.Actions.Push = actionSourceFromBool(s.Pushed)
	}
	if s.Actions.PR == "" {
		s.Actions.PR = actionSourceFromBool(s.PRNumber > 0 || strings.TrimSpace(s.PRURL) != "")
	}
}

func (s *State) MergeAgent(agent *State, ingestedAt string) {
	if s == nil || agent == nil {
		return
	}
	agent.Normalize()
	if s.Policy == "" {
		s.Policy = agent.Policy
	}
	if s.PolicyVersion == 0 {
		s.PolicyVersion = agent.PolicyVersion
	}
	if s.Branch == "" && agent.Branch != "" {
		s.Branch = agent.Branch
	}
	if s.BaseBranch == "" && agent.BaseBranch != "" {
		s.BaseBranch = agent.BaseBranch
	}
	s.CommitSHAs = appendUnique(s.CommitSHAs, agent.CommitSHAs...)
	if agent.Pushed {
		s.Pushed = true
		if s.Actions.Push != ActionSourceFallback {
			s.Actions.Push = sourceOr(agent.Actions.Push, ActionSourceAgent)
		}
	}
	if agent.PRNumber > 0 {
		s.PRNumber = agent.PRNumber
	}
	if strings.TrimSpace(agent.PRURL) != "" {
		s.PRURL = agent.PRURL
	}
	if agent.PRAdopted {
		s.PRAdopted = true
	}
	if len(agent.CommitSHAs) > 0 && s.Actions.Commit != ActionSourceFallback {
		s.Actions.Commit = sourceOr(agent.Actions.Commit, ActionSourceAgent)
	}
	if (agent.PRNumber > 0 || agent.PRURL != "") && s.Actions.PR != ActionSourceFallback {
		s.Actions.PR = sourceOr(agent.Actions.PR, ActionSourceAgent)
	}
	if ingestedAt != "" {
		s.AgentIngestedAt = ingestedAt
	}
	s.Normalize()
}

func PromptInstruction(runner, branch, baseBranch string, expectPush, expectPR bool, disclosureCfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	policy := AgentManagedWithFallbackPolicy(runner, expectPR)
	prStep := "create or adopt an open pull request for this branch"
	if !expectPR {
		prStep = "adopt the existing pull request for this branch when present; do not create a duplicate PR"
	}
	gitStep := "commit only relevant non-secret changes and push the current branch"
	if !expectPush {
		gitStep = "commit only relevant non-secret changes and leave the current branch ready for Looper fallback push/PR reconciliation"
	}
	return strings.Join([]string{
		"Agent-managed git/PR lifecycle policy: " + policy.Name + ".",
		"Before finishing: inspect git status, staged and unstaged diffs, untracked files, and recent commit style; " + gitStep + "; " + prStep + ".",
		disclosurePromptInstruction(runner, disclosureCfg, agentRuntime, agentModel),
		"If a branch PR already exists, reuse it and preserve human-edited title/body while adding only missing labels, reviewers, or closing references requested by the run.",
		"Include a git_pr_lifecycle object in the final " + "__LOOPER_RESULT__" + " JSON with branch, baseBranch, commitShas, pushed, prNumber, prUrl, prAdopted, and actions {commit,push,pr}; use action source \"agent\" for actions you completed and \"none\" for actions still missing.",
		fmt.Sprintf("Expected lifecycle branch=%q baseBranch=%q expectPush=%t expectPR=%t fallbackAllowed=%t.", branch, baseBranch, expectPush, policy.ExpectPR, policy.AllowFallback),
	}, "\n")
}

func DisclosurePromptInstruction(runner string, cfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	return disclosurePromptInstruction(runner, cfg, agentRuntime, agentModel)
}

func disclosurePromptInstruction(runner string, cfg config.DisclosureConfig, agentRuntime string, agentModel string) string {
	if !cfg.Enabled {
		return "Looper disclosure stamping is disabled by configuration; do not add looper Generated-By trailers, Markdown disclosure footers, or hidden looper stamp markers to generated external content."
	}
	stamper := disclosure.Stamper{Config: cfg, Version: version.Current().Version, Agent: strings.TrimSpace(agentRuntime), Model: agentModel}
	commitTrailer := stamper.CommitMessage("", runner)
	markdownChannel := disclosure.ChannelPullRequest
	if !cfg.Channels.PullRequest {
		markdownChannel = disclosure.ChannelIssueComment
	}
	markdownFooter := strings.TrimPrefix(stamper.Markdown("", runner, markdownChannel), disclosure.Marker+"\n")

	parts := []string{"Disclose looper-generated external content only for the disclosure channels enabled by configuration."}
	if cfg.Channels.GitCommit {
		parts = append(parts, "For commits, keep commit subjects unchanged and add a commit body trailer like `"+commitTrailer+"`.")
	} else {
		parts = append(parts, "Do not add looper Generated-By trailers to commit messages.")
	}
	if cfg.Channels.PullRequest || cfg.Channels.IssueComment {
		channels := []string{}
		if cfg.Channels.PullRequest {
			channels = append(channels, "PR bodies")
		}
		if cfg.Channels.IssueComment {
			channels = append(channels, "issue bodies or normal comments")
		}
		parts = append(parts, "For generated "+strings.Join(channels, " and ")+", add this Markdown footer: `<!-- looper:stamp v=1 -->` followed by `"+markdownFooter+"`.")
	}
	if !cfg.Channels.PullRequest {
		parts = append(parts, "Do not add looper Markdown disclosure footers to PR bodies.")
	}
	if !cfg.Channels.IssueComment {
		parts = append(parts, "Do not add looper Markdown disclosure footers to issue bodies or normal comments.")
	}
	if cfg.Channels.ReviewComment {
		if cfg.Channels.InlineCommentVisible {
			parts = append(parts, "For inline review comments, add the hidden `<!-- looper:stamp v=1 -->` marker followed by the visible Markdown disclosure footer.")
		} else {
			parts = append(parts, "For inline review comments, use only the hidden `<!-- looper:stamp v=1 -->` marker.")
		}
	} else {
		parts = append(parts, "Do not add looper disclosure footers or hidden looper stamp markers to inline review comments.")
	}
	parts = append(parts, "Do not include hostname, username, local paths, IP/MAC addresses, env vars, tokens, endpoints, or machine identifiers.")
	return strings.Join(parts, " ")
}

func actionSourceFromBool(done bool) string {
	if done {
		return ActionSourceAgent
	}
	return ActionSourceNone
}

func sourceOr(value, fallback string) string {
	if strings.TrimSpace(value) != "" && value != ActionSourceNone {
		return value
	}
	return fallback
}

func compact(values []string) []string {
	return appendUnique(nil, values...)
}

func appendUnique(dst []string, values ...string) []string {
	seen := map[string]bool{}
	for _, value := range dst {
		seen[value] = true
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		dst = append(dst, value)
	}
	return dst
}
