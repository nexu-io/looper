package planestrict

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/planeprotocol"
)

const maxResponseBytes = 4 << 20

type Credentials struct {
	BindingID     planeprotocol.UUID
	KeyRevision   uint64
	PrivateKey    ed25519.PrivateKey
	NodeID        string
	SessionID     planeprotocol.UUID
	InstanceNonce [16]byte
}

type Client struct {
	baseURL     *url.URL
	workspace   string
	projectID   string
	credentials Credentials
	httpClient  *http.Client
	now         func() time.Time
	rand        io.Reader
}

type Dispatch struct {
	ID                  string     `json:"id"`
	IssueID             string     `json:"issue_id"`
	Revision            uint64     `json:"revision"`
	StateVersion        uint64     `json:"state_version"`
	State               string     `json:"state"`
	Health              string     `json:"health"`
	RequestedMode       string     `json:"requested_mode"`
	ActiveRole          string     `json:"active_role"`
	OwnerMemberID       string     `json:"owner_member_id"`
	NodeID              string     `json:"node_id"`
	NodeBindingID       string     `json:"node_binding_id"`
	ExecutionAttemptID  *string    `json:"execution_attempt_id"`
	FencingToken        uint64     `json:"fencing_token"`
	ClaimLeaseExpiresAt *time.Time `json:"claim_lease_expires_at"`
}

type InboxResponse struct {
	Dispatches       []Dispatch `json:"dispatches"`
	IntegrationState string     `json:"integration_state"`
}

type MutationResponse struct {
	Dispatch Dispatch `json:"dispatch"`
	Claimed  bool     `json:"claimed,omitempty"`
}

type RoleQuestionOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Impact string `json:"impact"`
}

type RoleQuestion struct {
	ID                     string               `json:"id"`
	Question               string               `json:"question"`
	Context                string               `json:"context"`
	Options                []RoleQuestionOption `json:"options,omitempty"`
	RecommendedOption      string               `json:"recommended_option,omitempty"`
	RecommendationReason   string               `json:"recommendation_reason,omitempty"`
	DesignDocumentRequired bool                 `json:"design_document_required,omitempty"`
}

type RoleRequestInput struct {
	LoopID           string
	DecisionRevision int
	Role             string
	BriefSummary     string
	Questions        []RoleQuestion
}

type HandoffInput struct {
	ApprovalActorMemberID string
	ApprovalCommentID     string
	SpecContentHash       string
	SpecRevision          int
}

type RoleRequestResponse struct {
	RoleRequest struct {
		ID                 string    `json:"id"`
		Role               string    `json:"role"`
		Status             string    `json:"status"`
		RequestCommentID   string    `json:"request_comment_id"`
		EligibleMemberID   string    `json:"eligible_member_id"`
		EligibleMemberName string    `json:"eligible_member_name"`
		CreatedAt          time.Time `json:"created_at"`
		Created            bool      `json:"created"`
	} `json:"role_request"`
}

type RoleRequestMessage struct {
	ID               string         `json:"id"`
	ClientMessageID  string         `json:"client_message_id"`
	Kind             string         `json:"kind"`
	Body             string         `json:"body"`
	DeliveryState    string         `json:"delivery_state"`
	Evaluation       map[string]any `json:"evaluation"`
	InReplyToMessage *string        `json:"in_reply_to_message_id"`
	CommentID        *string        `json:"comment_id"`
	CreatedAt        time.Time      `json:"created_at"`
}

type PendingRoleMessage struct {
	RoleRequest struct {
		ID                string         `json:"id"`
		Role              string         `json:"role"`
		Status            string         `json:"status"`
		ConversationState string         `json:"conversation_state"`
		Questions         []RoleQuestion `json:"questions"`
		Resolution        map[string]any `json:"resolution"`
	} `json:"role_request"`
	Message RoleRequestMessage   `json:"message"`
	History []RoleRequestMessage `json:"history"`
}

type PendingRoleMessagesResponse struct {
	Pending []PendingRoleMessage `json:"pending"`
}

type RoleQuestionEvaluation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Answer string `json:"answer,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type RoleMessageEvaluation struct {
	Resolved  bool                     `json:"resolved"`
	Questions []RoleQuestionEvaluation `json:"questions"`
}

type RoleMessageReplyInput struct {
	ClientMessageID     string
	InReplyToMessageID  string
	ProcessedMessageIDs []string
	Reply               string
	Evaluation          RoleMessageEvaluation
}

type RoleMessageReplyResponse struct {
	Message RoleRequestMessage `json:"message"`
	Created bool               `json:"created"`
}

type APIError struct {
	StatusCode int
	Code       string
	Detail     string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("Plane strict API %s (%d): %s", e.Code, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("Plane strict API %s (%d)", e.Code, e.StatusCode)
}

type Option func(*Client)

func WithHTTPClient(client *http.Client) Option {
	return func(value *Client) {
		if client != nil {
			value.httpClient = client
		}
	}
}

func WithClock(now func() time.Time) Option {
	return func(value *Client) {
		if now != nil {
			value.now = now
		}
	}
}

func WithRandom(reader io.Reader) Option {
	return func(value *Client) {
		if reader != nil {
			value.rand = reader
		}
	}
}

func NewClient(baseURL, workspace, projectID string, credentials Credentials, options ...Option) (*Client, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Plane strict client: absolute base URL is required")
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		return nil, errors.New("Plane strict client: HTTPS is required outside localhost")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Plane strict client: base URL cannot contain query or fragment")
	}
	if strings.TrimSpace(workspace) == "" || strings.TrimSpace(projectID) == "" {
		return nil, errors.New("Plane strict client: workspace and project ID are required")
	}
	if len(credentials.PrivateKey) != ed25519.PrivateKeySize || credentials.KeyRevision == 0 || strings.TrimSpace(credentials.NodeID) == "" {
		return nil, errors.New("Plane strict client: complete Node credentials are required")
	}
	client := &Client{
		baseURL: parsed, workspace: strings.TrimSpace(workspace), projectID: strings.TrimSpace(projectID),
		credentials: credentials,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:  time.Now,
		rand: rand.Reader,
	}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client, nil
}

func (c *Client) Inbox(ctx context.Context, cursor string) (InboxResponse, error) {
	if err := c.openSession(ctx); err != nil {
		return InboxResponse{}, err
	}
	query := url.Values{
		"cursor":         {cursor},
		"node_id":        {c.credentials.NodeID},
		"session_id":     {UUIDString(c.credentials.SessionID)},
		"instance_nonce": {base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:])},
	}
	var response InboxResponse
	err := c.do(ctx, http.MethodGet, c.projectPath("looper", "dispatch", "inbox"), query, nil, nil, &response)
	return response, err
}

func (c *Client) Claim(ctx context.Context, dispatch Dispatch, idempotencyKey string) (MutationResponse, error) {
	stateVersion := dispatch.StateVersion
	body := map[string]any{
		"expected_state_version": dispatch.StateVersion,
		"claim_idempotency_key":  idempotencyKey,
		"session_id":             UUIDString(c.credentials.SessionID),
		"instance_nonce":         base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
	}
	var response MutationResponse
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "dispatch", dispatch.ID, "claim"), nil, body, &signingContext{
		dispatchID: dispatch.ID, dispatchRevision: dispatch.Revision, stateVersion: &stateVersion,
	}, &response)
	return response, err
}

func (c *Client) Transition(ctx context.Context, dispatch Dispatch, nextState string, waitKind *string) (MutationResponse, error) {
	if dispatch.ExecutionAttemptID == nil {
		return MutationResponse{}, errors.New("Plane strict transition: execution attempt is missing")
	}
	stateVersion, fencingToken := dispatch.StateVersion, dispatch.FencingToken
	body := map[string]any{
		"expected_state_version": dispatch.StateVersion,
		"execution_attempt_id":   *dispatch.ExecutionAttemptID,
		"fencing_token":          dispatch.FencingToken,
		"state":                  nextState,
		"wait_kind":              waitKind,
		"session_id":             UUIDString(c.credentials.SessionID),
		"instance_nonce":         base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
	}
	if nextState == "awaiting_human" || nextState == "completed" || nextState == "failed" || nextState == "confirmed_stopped" {
		body["termination_summary"] = map[string]any{
			"exit_status": 0, "exited_at": c.now().UTC().Format(time.RFC3339Nano),
			"process_group_empty": true, "evidence": "looper-runner-wait",
		}
	}
	var response MutationResponse
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "dispatch", dispatch.ID, "transition"), nil, body, &signingContext{
		dispatchID: dispatch.ID, dispatchRevision: dispatch.Revision, stateVersion: &stateVersion,
		attemptID: dispatch.ExecutionAttemptID, fencingToken: &fencingToken,
	}, &response)
	return response, err
}

func (c *Client) Handoff(ctx context.Context, dispatch Dispatch, input HandoffInput) (MutationResponse, error) {
	if dispatch.ExecutionAttemptID == nil {
		return MutationResponse{}, errors.New("Plane strict handoff: execution attempt is missing")
	}
	stateVersion, fencingToken := dispatch.StateVersion, dispatch.FencingToken
	body := map[string]any{
		"expected_state_version":   dispatch.StateVersion,
		"execution_attempt_id":     *dispatch.ExecutionAttemptID,
		"fencing_token":            dispatch.FencingToken,
		"session_id":               UUIDString(c.credentials.SessionID),
		"instance_nonce":           base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
		"approval_actor_member_id": input.ApprovalActorMemberID,
		"approval_comment_id":      input.ApprovalCommentID,
		"spec_content_hash":        input.SpecContentHash,
		"spec_revision":            input.SpecRevision,
	}
	var response MutationResponse
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "dispatch", dispatch.ID, "handoff"), nil, body, &signingContext{
		dispatchID: dispatch.ID, dispatchRevision: dispatch.Revision, stateVersion: &stateVersion,
		attemptID: dispatch.ExecutionAttemptID, fencingToken: &fencingToken,
	}, &response)
	return response, err
}

func (c *Client) CreateRoleRequest(ctx context.Context, dispatch Dispatch, input RoleRequestInput) (RoleRequestResponse, error) {
	if dispatch.ExecutionAttemptID == nil {
		return RoleRequestResponse{}, errors.New("Plane strict role request: execution attempt is missing")
	}
	stateVersion, fencingToken := dispatch.StateVersion, dispatch.FencingToken
	body := map[string]any{
		"expected_state_version": dispatch.StateVersion,
		"execution_attempt_id":   *dispatch.ExecutionAttemptID,
		"fencing_token":          dispatch.FencingToken,
		"session_id":             UUIDString(c.credentials.SessionID),
		"instance_nonce":         base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
		"loop_id":                input.LoopID,
		"decision_revision":      input.DecisionRevision,
		"role":                   input.Role,
		"brief_summary":          input.BriefSummary,
		"questions":              input.Questions,
		"termination_summary": map[string]any{
			"exit_status": 0, "exited_at": c.now().UTC().Format(time.RFC3339Nano),
			"process_group_empty": true, "evidence": "looper-runner-wait",
		},
	}
	var response RoleRequestResponse
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "dispatch", dispatch.ID, "role-requests"), nil, body, &signingContext{
		dispatchID: dispatch.ID, dispatchRevision: dispatch.Revision, stateVersion: &stateVersion,
		attemptID: dispatch.ExecutionAttemptID, fencingToken: &fencingToken,
	}, &response)
	return response, err
}

func (c *Client) PendingRoleMessages(ctx context.Context, dispatch Dispatch) (PendingRoleMessagesResponse, error) {
	if dispatch.ExecutionAttemptID == nil {
		return PendingRoleMessagesResponse{}, errors.New("Plane strict role messages: execution attempt is missing")
	}
	stateVersion, fencingToken := dispatch.StateVersion, dispatch.FencingToken
	body := map[string]any{
		"expected_state_version": dispatch.StateVersion,
		"execution_attempt_id":   *dispatch.ExecutionAttemptID,
		"fencing_token":          dispatch.FencingToken,
		"session_id":             UUIDString(c.credentials.SessionID),
		"instance_nonce":         base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
	}
	var response PendingRoleMessagesResponse
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "dispatch", dispatch.ID, "role-requests", "messages", "pending"), nil, body, &signingContext{
		dispatchID: dispatch.ID, dispatchRevision: dispatch.Revision, stateVersion: &stateVersion,
		attemptID: dispatch.ExecutionAttemptID, fencingToken: &fencingToken,
	}, &response)
	return response, err
}

func (c *Client) ReplyRoleMessage(ctx context.Context, dispatch Dispatch, roleRequestID string, input RoleMessageReplyInput) (RoleMessageReplyResponse, error) {
	if dispatch.ExecutionAttemptID == nil {
		return RoleMessageReplyResponse{}, errors.New("Plane strict role message reply: execution attempt is missing")
	}
	stateVersion, fencingToken := dispatch.StateVersion, dispatch.FencingToken
	body := map[string]any{
		"expected_state_version": dispatch.StateVersion,
		"execution_attempt_id":   *dispatch.ExecutionAttemptID,
		"fencing_token":          dispatch.FencingToken,
		"session_id":             UUIDString(c.credentials.SessionID),
		"instance_nonce":         base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
		"client_message_id":      input.ClientMessageID,
		"in_reply_to_message_id": input.InReplyToMessageID,
		"processed_message_ids":  input.ProcessedMessageIDs,
		"reply":                  input.Reply,
		"evaluation":             input.Evaluation,
	}
	var response RoleMessageReplyResponse
	err := c.do(ctx, http.MethodPost, c.projectPath("looper", "dispatch", dispatch.ID, "role-requests", roleRequestID, "messages"), nil, body, &signingContext{
		dispatchID: dispatch.ID, dispatchRevision: dispatch.Revision, stateVersion: &stateVersion,
		attemptID: dispatch.ExecutionAttemptID, fencingToken: &fencingToken,
	}, &response)
	return response, err
}

func (c *Client) openSession(ctx context.Context) error {
	body := map[string]any{
		"session_id":     UUIDString(c.credentials.SessionID),
		"instance_nonce": base64.RawURLEncoding.EncodeToString(c.credentials.InstanceNonce[:]),
	}
	var response struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	}
	if err := c.do(ctx, http.MethodPost, c.projectPath("looper", "nodes", "session"), nil, body, nil, &response); err != nil {
		return fmt.Errorf("open Plane Node session: %w", err)
	}
	if response.SessionID != UUIDString(c.credentials.SessionID) || response.State != "active" {
		return errors.New("Plane Node session response does not match this instance")
	}
	return nil
}

type signingContext struct {
	dispatchID       string
	dispatchRevision uint64
	stateVersion     *uint64
	attemptID        *string
	fencingToken     *uint64
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, signed *signingContext, output any) error {
	var rawBody []byte
	var err error
	if body != nil {
		rawBody, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	requestURL := *c.baseURL
	requestURL.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	requestURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), bytes.NewReader(rawBody))
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("User-Agent", "looper-strict-dispatch/1.0")
	header, err := c.signatureHeader(method, requestURL.EscapedPath(), requestURL.RawQuery, rawBody, signed)
	if err != nil {
		return err
	}
	request.Header.Set("Looper-Signature", header)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	rawResponse, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(rawResponse) > maxResponseBytes {
		return errors.New("Plane strict API response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var value struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(rawResponse, &value)
		return &APIError{StatusCode: response.StatusCode, Code: value.Error, Detail: value.Detail}
	}
	if output == nil || len(rawResponse) == 0 {
		return nil
	}
	if err := json.Unmarshal(rawResponse, output); err != nil {
		return fmt.Errorf("decode Plane strict API response: %w", err)
	}
	return nil
}

func (c *Client) signatureHeader(method, path, query string, body []byte, signed *signingContext) (string, error) {
	var nonce [16]byte
	if _, err := io.ReadFull(c.rand, nonce[:]); err != nil {
		return "", fmt.Errorf("generate signed request nonce: %w", err)
	}
	bodyHash := sha256.Sum256(body)
	value := planeprotocol.NodeRequest{
		Method: method, Path: path, Query: query, BodySHA256: bodyHash,
		BindingID: c.credentials.BindingID, KeyRevision: c.credentials.KeyRevision,
		TimestampMS: c.now().UnixMilli(), Nonce: nonce,
	}
	if signed != nil {
		dispatchID, err := parseUUID(signed.dispatchID)
		if err != nil {
			return "", fmt.Errorf("dispatch ID: %w", err)
		}
		value.DispatchID = &dispatchID
		value.DispatchRevision = &signed.dispatchRevision
		value.StateVersion = signed.stateVersion
		if signed.attemptID != nil {
			attemptID, err := parseUUID(*signed.attemptID)
			if err != nil {
				return "", fmt.Errorf("execution attempt ID: %w", err)
			}
			value.ExecutionAttemptID = &attemptID
			value.FencingToken = signed.fencingToken
		}
	}
	payload, err := planeprotocol.EncodeNodeRequest(value)
	if err != nil {
		return "", err
	}
	digest, err := planeprotocol.DomainDigest(planeprotocol.NodeRequestProfile, payload)
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(c.credentials.PrivateKey, digest[:])
	var fixedSignature [64]byte
	copy(fixedSignature[:], signature)
	return planeprotocol.FormatSignatureHeader(planeprotocol.SignatureHeader{
		BindingID: c.credentials.BindingID, KeyRevision: c.credentials.KeyRevision,
		TimestampMS: value.TimestampMS, Nonce: nonce, Signature: fixedSignature,
	}), nil
}

func (c *Client) projectPath(parts ...string) string {
	values := []string{"", "api", "workspaces", url.PathEscape(c.workspace), "projects", url.PathEscape(c.projectID)}
	for _, part := range parts {
		values = append(values, url.PathEscape(part))
	}
	return strings.Join(values, "/") + "/"
}

func parseUUID(value string) (planeprotocol.UUID, error) {
	var result planeprotocol.UUID
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return result, errors.New("invalid UUID")
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(raw) != len(result) {
		return result, errors.New("invalid UUID")
	}
	copy(result[:], raw)
	return result, nil
}

func UUIDString(value planeprotocol.UUID) string {
	raw := hex.EncodeToString(value[:])
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
}

func ParseKeyRevision(value string) (uint64, error) {
	revision, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil || revision == 0 {
		return 0, errors.New("key revision must be a positive integer")
	}
	return revision, nil
}
