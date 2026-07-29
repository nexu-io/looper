package cliapp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	networkclient "github.com/nexu-io/looper/internal/network/client"
	"github.com/nexu-io/looper/internal/network/protocol"
	"github.com/nexu-io/looper/internal/planeprotocol"
	"github.com/spf13/cobra"
)

const planeLinkMediaType = "application/vnd.looper.link-request+cbor;v=1"

type planeAPIError struct {
	Status int
	Code   string
	Detail string
}

func (e *planeAPIError) Error() string {
	if e.Code != "" && e.Detail != "" {
		return fmt.Sprintf("Plane returned HTTP %d (%s): %s", e.Status, e.Code, e.Detail)
	}
	if e.Code != "" {
		return fmt.Sprintf("Plane returned HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("Plane returned HTTP %d", e.Status)
}

type planeLinkIdentity struct {
	ID string `json:"id"`
}

type planeLinkBinding struct {
	ID          string `json:"id"`
	MemberID    string `json:"member_id"`
	NodeID      string `json:"node_id"`
	State       string `json:"state"`
	KeyRevision uint64 `json:"revision"`
}

type planeConnectExchange struct {
	ConnectionID string `json:"connection_id"`
	Status       string `json:"status"`
	Workspace    string `json:"workspace"`
	WorkspaceID  string `json:"workspace_id"`
	ProjectID    string `json:"project_id"`
	MemberID     string `json:"member_id"`
	BindingID    string `json:"binding_id"`
	NodeID       string `json:"node_id"`
}

type planeLinkProofIdentity struct {
	WorkspaceID string
	ProjectID   string
	MemberID    string
}

func (r *commandRuntime) planeConnect(cmd *cobra.Command, args []string) error {
	connectCode := strings.TrimSpace(getStringFlag(cmd, "code"))
	if connectCode == "" {
		return errors.New("plane connect requires --code from the Plane connection page")
	}
	planeOrigin, err := planeWebOrigin(args[0])
	if err != nil {
		return err
	}
	exchangeBody, _ := json.Marshal(map[string]string{"code": connectCode})
	exchange := planeConnectExchange{}
	if err := r.planeRawRequest(cmd.Context(), planeOrigin, "", http.MethodPost, "/api/looper/connections/exchange/", "application/json", exchangeBody, &exchange); err != nil {
		return fmt.Errorf("exchange Plane connection code: %w", err)
	}

	homeDir, err := r.homeDir()
	if err != nil {
		return err
	}
	networkState, err := networkclient.LoadState(networkclient.DefaultStatePath(homeDir))
	if err != nil {
		return fmt.Errorf("plane connect requires this computer to join loopernet first: %w; run the network join command shown by your team setup, then repeat this command", err)
	}
	loaded, providerIndex, provider, err := r.preparePlaneConnectProvider(cmd, planeOrigin, exchange)
	if err != nil {
		return err
	}
	privateKeyFile := filepath.Join(homeDir, ".looper", "runtime", "plane-"+safeFileComponent(provider.ID)+".pem")
	pendingPrivateKeyFile := privateKeyFile + ".pending"
	if exchange.Status == "completed" {
		if exchange.NodeID != networkState.NodeID || provider.StrictDispatch == nil ||
			provider.StrictDispatch.BindingID != exchange.BindingID ||
			provider.StrictDispatch.NodeID != exchange.NodeID {
			return errors.New("this Plane connection is complete on a different or unconfigured local Node")
		}
		return r.writePlaneConnectResult(cmd, provider.ID, exchange, networkState.NodeName)
	}
	if exchange.Status == "binding_created" {
		if exchange.BindingID == "" || exchange.NodeID != networkState.NodeID {
			return errors.New("the pending Plane connection belongs to a different local Node")
		}
		if provider.StrictDispatch != nil && strings.TrimSpace(provider.StrictDispatch.BindingID) != "" {
			if provider.StrictDispatch.BindingID != exchange.BindingID || provider.StrictDispatch.NodeID != exchange.NodeID {
				return errors.New("this Plane provider is already connected to a different computer")
			}
			if configuredKey := strings.TrimSpace(provider.StrictDispatch.PrivateKeyFile); configuredKey != "" {
				privateKeyFile = configuredKey
			}
		}
		if _, readErr := r.readFile(privateKeyFile); readErr != nil {
			if !os.IsNotExist(readErr) {
				return fmt.Errorf("inspect Plane private key %s: %w", privateKeyFile, readErr)
			}
			if _, pendingErr := r.readFile(pendingPrivateKeyFile); pendingErr != nil {
				return fmt.Errorf("resume Plane connection requires the existing private key %s: %w", privateKeyFile, pendingErr)
			}
			if err := renameFile(pendingPrivateKeyFile, privateKeyFile); err != nil {
				return fmt.Errorf("promote pending Plane private key: %w", err)
			}
		}
		strict := config.PlaneStrictDispatchConfig{
			Enabled: true, BaseURL: planeOrigin, NodeID: exchange.NodeID,
			BindingID: exchange.BindingID, KeyRevision: 1, PrivateKeyFile: privateKeyFile,
		}
		if err := r.writePlaneStrictProviderConfig(loaded, providerIndex, strict); err != nil {
			return err
		}
		return r.finishPlaneConnection(cmd, planeOrigin, connectCode, provider.ID, exchange, networkState.NodeName)
	}
	if exchange.Status != "" && exchange.Status != "created" && exchange.Status != "cli_connected" {
		return fmt.Errorf("Plane connection is not available (status %s)", exchange.Status)
	}
	if provider.StrictDispatch != nil && strings.TrimSpace(provider.StrictDispatch.BindingID) != "" {
		return errors.New("this Plane provider is already connected to a computer")
	}
	if _, readErr := r.readFile(pendingPrivateKeyFile); readErr == nil {
		if err := r.removeFile(pendingPrivateKeyFile); err != nil {
			return fmt.Errorf("remove an uncommitted Plane private key: %w", err)
		}
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("inspect pending Plane private key: %w", readErr)
	}
	if _, readErr := r.readFile(privateKeyFile); readErr == nil {
		return fmt.Errorf("refusing to replace existing Plane private key %s", privateKeyFile)
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("inspect Plane private key: %w", readErr)
	}
	linkBody, privateKey, err := r.buildPlaneLinkRequest(
		cmd.Context(),
		networkState,
		planeLinkProofIdentity{
			WorkspaceID: exchange.WorkspaceID,
			ProjectID:   exchange.ProjectID,
			MemberID:    exchange.MemberID,
		},
	)
	if err != nil {
		return err
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(privateKeyFile), 0o700); err != nil {
		return err
	}
	if err := r.writeFile(pendingPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		return err
	}
	linkResponse := struct {
		ConnectionID string           `json:"connection_id"`
		Binding      planeLinkBinding `json:"binding"`
	}{}
	if err := r.planeRawRequestWithHeaders(
		cmd.Context(), planeOrigin, "", http.MethodPost, "/api/looper/connections/link/",
		planeLinkMediaType, linkBody,
		map[string]string{"X-Looper-Connect-Code": connectCode, "X-Looper-Node-Name": networkState.NodeName},
		&linkResponse,
	); err != nil {
		var apiErr *planeAPIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			_ = r.removeFile(pendingPrivateKeyFile)
			return fmt.Errorf("create self-service Plane Node binding: %w", err)
		}
		return fmt.Errorf("Plane may have created the binding, so the pending private key was kept at %s: %w; rerun the same Plane command to reconcile safely", pendingPrivateKeyFile, err)
	}
	if err := renameFile(pendingPrivateKeyFile, privateKeyFile); err != nil {
		return fmt.Errorf("binding created and pending private key kept at %s, but promote it: %w; rerun the same Plane command to recover", pendingPrivateKeyFile, err)
	}
	strict := config.PlaneStrictDispatchConfig{
		Enabled: true, BaseURL: planeOrigin, NodeID: networkState.NodeID,
		BindingID: linkResponse.Binding.ID, KeyRevision: 1, PrivateKeyFile: privateKeyFile,
	}
	if err := r.writePlaneStrictProviderConfig(loaded, providerIndex, strict); err != nil {
		return fmt.Errorf("binding created and private key kept at %s, but save provider config: %w; rerun the same Plane command to recover", privateKeyFile, err)
	}
	exchange.BindingID = linkResponse.Binding.ID
	exchange.NodeID = linkResponse.Binding.NodeID
	exchange.Status = "binding_created"
	return r.finishPlaneConnection(cmd, planeOrigin, connectCode, provider.ID, exchange, networkState.NodeName)
}

func (r *commandRuntime) preparePlaneConnectProvider(cmd *cobra.Command, planeOrigin string, exchange planeConnectExchange) (config.LoadedFileConfig, int, config.ProviderConfig, error) {
	wanted := strings.TrimSpace(getStringFlag(cmd, "provider"))
	loaded, loadErr := r.loadConfig()
	if loadErr == nil {
		if index, provider, matchErr := matchingPlaneConnectProvider(loaded.Config, planeOrigin, exchange, wanted); matchErr == nil {
			return loaded, index, provider, nil
		}
	}

	cwd, err := r.getwd()
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, fmt.Errorf("determine current directory for Plane auto-configuration: %w", err)
	}
	configPath, err := r.resolveBootstrapConfigPath(cwd)
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, err
	}
	if loadErr != nil {
		if _, statErr := os.Stat(configPath); statErr == nil || !os.IsNotExist(statErr) {
			return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, loadErr
		}
	}

	projectPath := strings.TrimSpace(getStringFlag(cmd, "project-path"))
	if projectPath == "" {
		projectPath = cwd
	}
	projectPath, err = filepath.Abs(projectPath)
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, fmt.Errorf("resolve Plane project path: %w", err)
	}
	if info, statErr := os.Stat(projectPath); statErr != nil || !info.IsDir() {
		if statErr == nil {
			statErr = errors.New("not a directory")
		}
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, fmt.Errorf("Plane project path %s is unavailable: %w", projectPath, statErr)
	}
	codeRepo := strings.TrimSpace(getStringFlag(cmd, "code-repo"))
	if codeRepo == "" {
		codeRepo = r.detectBootstrapOriginRepo(cmd.Context(), projectPath)
	}
	if codeRepo == "" {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, fmt.Errorf("cannot auto-configure this Plane project from %s: run the command inside its GitHub checkout, or pass --project-path and --code-repo owner/repo", projectPath)
	}
	tokenEnv := strings.TrimSpace(getStringFlag(cmd, "plane-token-env"))
	if tokenEnv == "" {
		tokenEnv = defaultPlaneBootstrapToken
	}
	if !environmentNamePattern.MatchString(tokenEnv) {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, fmt.Errorf("--plane-token-env %q is not a valid environment variable name", tokenEnv)
	}
	plan := bootstrapConfigPlan{
		Provider: bootstrapProviderPlane, ProjectPath: projectPath, CodeRepo: codeRepo,
		TriggerLabel: strings.TrimSpace(getStringFlag(cmd, "trigger-label")),
		PlaneBaseURL: strings.TrimRight(planeOrigin, "/") + "/api/v1", PlaneWorkspace: exchange.Workspace,
		PlaneProject: exchange.ProjectID, PlaneTokenEnv: tokenEnv,
	}
	generatedProviderID := planeBootstrapProviderID(exchange.Workspace)
	if wanted != "" && wanted != generatedProviderID {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, fmt.Errorf("provider %q is not configured for this Plane project; omit --provider to create %q automatically", wanted, generatedProviderID)
	}
	if _, _, err := r.ensureBootstrapConfig(configPath, cwd, plan); err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, fmt.Errorf("auto-configure Plane project: %w", err)
	}
	loaded, err = r.loadConfig()
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, err
	}
	index, provider, err := matchingPlaneConnectProvider(loaded.Config, planeOrigin, exchange, wanted)
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, err
	}
	if strings.TrimSpace(os.Getenv(tokenEnv)) == "" && !getBoolFlag(cmd, "json") {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Configured Plane project %s. Before dispatching work, export %s with your Plane API key and restart looperd.\n", exchange.Workspace, tokenEnv)
	}
	return loaded, index, provider, nil
}

func (r *commandRuntime) finishPlaneConnection(cmd *cobra.Command, planeOrigin, connectCode, providerID string, exchange planeConnectExchange, nodeName string) error {
	if err := r.daemonRestartForBootstrap(cmd); err != nil {
		if strings.Contains(err.Error(), `unknown field "strictDispatch"`) {
			return fmt.Errorf("binding and local configuration are ready, but the installed looperd is too old for Plane dispatch: %w; run `looper upgrade --daemon`, then rerun the same Plane command", err)
		}
		return fmt.Errorf("binding and local configuration are ready, but restart Looper daemon: %w; rerun the same Plane command to recover", err)
	}
	completeBody, _ := json.Marshal(map[string]string{"code": connectCode})
	var completeErr error
	for attempt := 0; attempt < 31; attempt++ {
		completeErr = r.planeRawRequest(cmd.Context(), planeOrigin, "", http.MethodPost, "/api/looper/connections/complete/", "application/json", completeBody, nil)
		if completeErr == nil {
			break
		}
		var apiErr *planeAPIError
		if !errors.As(completeErr, &apiErr) || apiErr.Code != "node_not_ready" {
			return fmt.Errorf("complete Plane connection: %w", completeErr)
		}
		if attempt < 30 {
			r.sleep(2 * time.Second)
		}
	}
	if completeErr != nil {
		return fmt.Errorf("Looper daemon restarted, but its signed Plane inbox did not become ready within 60 seconds: %w; rerun the same Plane command to continue checking", completeErr)
	}
	return r.writePlaneConnectResult(cmd, providerID, exchange, nodeName)
}

func (r *commandRuntime) writePlaneConnectResult(cmd *cobra.Command, providerID string, exchange planeConnectExchange, nodeName string) error {
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providerId": providerID, "connectionId": exchange.ConnectionID, "bindingId": exchange.BindingID, "connected": true})
	}
	printSection(cmd.OutOrStdout(), "Plane Looper connected", [][2]any{
		{"providerId", providerID}, {"ownerMemberId", exchange.MemberID}, {"node", nodeName},
		{"bindingId", exchange.BindingID}, {"daemonReloaded", true}, {"next", "Return to Plane and dispatch a work item to your Looper."},
	})
	return nil
}

func matchingPlaneConnectProvider(cfg config.Config, planeOrigin string, exchange planeConnectExchange, wanted string) (int, config.ProviderConfig, error) {
	matchIndex := -1
	for index, provider := range cfg.Providers {
		if provider.Kind != config.ProviderKindPlane || provider.Workspace == nil || provider.ProjectID == nil {
			continue
		}
		origin, err := planeWebOrigin(provider.BaseURL)
		if err != nil || origin != planeOrigin || *provider.Workspace != exchange.Workspace || *provider.ProjectID != exchange.ProjectID {
			continue
		}
		if strings.TrimSpace(wanted) != "" && provider.ID != strings.TrimSpace(wanted) {
			continue
		}
		if matchIndex != -1 {
			return 0, config.ProviderConfig{}, errors.New("multiple Plane providers match this project; pass --provider")
		}
		matchIndex = index
	}
	if matchIndex == -1 {
		return 0, config.ProviderConfig{}, errors.New("no configured Plane provider matches this connection; run `looper bootstrap` first")
	}
	return matchIndex, cfg.Providers[matchIndex], nil
}

func (r *commandRuntime) buildPlaneLinkRequest(
	ctx context.Context,
	networkState networkclient.LocalState,
	identity planeLinkProofIdentity,
) ([]byte, ed25519.PrivateKey, error) {
	workspaceID, err := parsePlaneProtocolUUID(identity.WorkspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("Plane workspace id: %w", err)
	}
	projectID, err := parsePlaneProtocolUUID(identity.ProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("Plane project id: %w", err)
	}
	memberID, err := parsePlaneProtocolUUID(identity.MemberID)
	if err != nil {
		return nil, nil, fmt.Errorf("Plane member id: %w", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	publicHash := sha256.Sum256(publicKey)
	audience := "plane:" + identity.WorkspaceID
	challengeResponse, err := networkclient.New(
		networkState.URL,
		networkState.NodeToken,
		r.httpClient(),
	).LinkChallenge(ctx, protocol.LinkChallengeRequest{
		PublicKeySHA256: base64.RawURLEncoding.EncodeToString(publicHash[:]),
		Audience:        audience,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("request loopernet Plane challenge: %w", err)
	}
	challengeEnvelope, err := base64.RawURLEncoding.Strict().DecodeString(challengeResponse.Challenge)
	if err != nil {
		return nil, nil, fmt.Errorf("decode loopernet challenge: %w", err)
	}
	envelope, err := planeprotocol.DecodeEnvelope(challengeEnvelope)
	if err != nil {
		return nil, nil, fmt.Errorf("decode loopernet challenge envelope: %w", err)
	}
	challenge, err := planeprotocol.DecodeLinkChallenge(envelope.Payload)
	if err != nil {
		return nil, nil, err
	}
	if challenge.NetworkID != networkState.NetworkID ||
		challenge.NodeID != networkState.NodeID ||
		challenge.PublicKeySHA256 != publicHash ||
		challenge.Audience != audience {
		return nil, nil, errors.New("loopernet challenge does not match this Node and Plane workspace")
	}
	challengeDigest, err := planeprotocol.DomainDigest(planeprotocol.LinkChallengeProfile, envelope.Payload)
	if err != nil {
		return nil, nil, err
	}
	var publicKeyFixed [32]byte
	copy(publicKeyFixed[:], publicKey)
	proofPayload, err := planeprotocol.EncodeLinkProof(planeprotocol.LinkProof{
		ChallengeSHA256: challengeDigest,
		PlaneWorkspace:  workspaceID,
		PlaneProject:    projectID,
		MemberID:        memberID,
		PublicKey:       publicKeyFixed,
	})
	if err != nil {
		return nil, nil, err
	}
	proofEnvelope, err := planeprotocol.SignEnvelope(
		privateKey,
		planeprotocol.LinkProofProfile,
		0,
		proofPayload,
	)
	if err != nil {
		return nil, nil, err
	}
	linkBody, err := planeprotocol.EncodeLinkRequest(challengeEnvelope, proofEnvelope)
	if err != nil {
		return nil, nil, err
	}
	return linkBody, privateKey, nil
}

func (r *commandRuntime) planeLink(cmd *cobra.Command, args []string) error {
	loaded, providerIndex, provider, token, strictBaseURL, err := r.planeCommandContext(args, getStringFlag(cmd, "strict-base-url"))
	if err != nil {
		return err
	}
	homeDir, err := r.homeDir()
	if err != nil {
		return err
	}
	networkState, err := networkclient.LoadState(networkclient.DefaultStatePath(homeDir))
	if err != nil {
		return fmt.Errorf("plane link requires a joined loopernet network: %w", err)
	}
	if provider.StrictDispatch != nil && strings.TrimSpace(provider.StrictDispatch.BindingID) != "" {
		return errors.New("Plane provider already has a strict binding; use `looper plane enable` after approval")
	}

	privateKeyFile := filepath.Join(homeDir, ".looper", "runtime", "plane-"+safeFileComponent(provider.ID)+".pem")
	if _, readErr := r.readFile(privateKeyFile); readErr == nil {
		return fmt.Errorf("refusing to replace existing Plane private key %s", privateKeyFile)
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("inspect Plane private key: %w", readErr)
	}
	user := planeLinkIdentity{}
	if err := r.planeJSONRequest(cmd.Context(), provider.BaseURL, token, http.MethodGet, "users/me", "", nil, &user); err != nil {
		return fmt.Errorf("read current Plane identity: %w", err)
	}
	workspace := planeLinkIdentity{}
	if err := r.planeJSONRequest(cmd.Context(), provider.BaseURL, token, http.MethodGet, "workspaces/"+url.PathEscape(*provider.Workspace), "", nil, &workspace); err != nil {
		return fmt.Errorf("read Plane workspace: %w", err)
	}
	linkBody, privateKey, err := r.buildPlaneLinkRequest(
		cmd.Context(),
		networkState,
		planeLinkProofIdentity{
			WorkspaceID: workspace.ID,
			ProjectID:   *provider.ProjectID,
			MemberID:    user.ID,
		},
	)
	if err != nil {
		return err
	}
	linkPath := fmt.Sprintf(
		"/api/workspaces/%s/projects/%s/looper/bindings/link/",
		url.PathEscape(*provider.Workspace),
		url.PathEscape(*provider.ProjectID),
	)
	binding := planeLinkBinding{}
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPost, linkPath, planeLinkMediaType, linkBody, &binding); err != nil {
		return fmt.Errorf("create Plane Node binding: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(privateKeyFile), 0o700); err != nil {
		return err
	}
	if err := r.writeFile(privateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		return err
	}
	strict := config.PlaneStrictDispatchConfig{
		Enabled: binding.State == "active", BaseURL: strictBaseURL, NodeID: networkState.NodeID,
		BindingID: binding.ID, KeyRevision: 1, PrivateKeyFile: privateKeyFile,
	}
	if err := r.writePlaneStrictProviderConfig(loaded, providerIndex, strict); err != nil {
		_ = r.removeFile(privateKeyFile)
		return err
	}
	next := "Run `looper daemon restart`."
	if binding.State != "active" {
		next = "This Plane server still uses legacy approval. Ask a project admin to approve the binding, then run `looper plane enable " + provider.ID + "`."
	}
	output := map[string]any{
		"providerId": provider.ID, "bindingId": binding.ID, "bindingState": binding.State,
		"nodeId": networkState.NodeID, "memberId": binding.MemberID, "privateKeyFile": privateKeyFile,
		"next": next,
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), output)
	}
	printSection(cmd.OutOrStdout(), "Plane Node linked", [][2]any{
		{"providerId", provider.ID}, {"bindingId", binding.ID}, {"bindingState", binding.State},
		{"nodeId", networkState.NodeID}, {"memberId", binding.MemberID}, {"privateKeyFile", privateKeyFile},
		{"next", output["next"]},
	})
	return nil
}

func (r *commandRuntime) planeEnable(cmd *cobra.Command, args []string) error {
	loaded, providerIndex, provider, token, strictBaseURL, err := r.planeCommandContext(args, "")
	if err != nil {
		return err
	}
	if provider.StrictDispatch == nil || strings.TrimSpace(provider.StrictDispatch.BindingID) == "" {
		return errors.New("Plane provider is not linked; run `looper plane link` first")
	}
	path := fmt.Sprintf("/api/workspaces/%s/projects/%s/looper/targets/", url.PathEscape(*provider.Workspace), url.PathEscape(*provider.ProjectID))
	var response struct {
		Targets []planeLinkBinding `json:"targets"`
	}
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodGet, path, "", nil, &response); err != nil {
		return fmt.Errorf("check Plane Node binding: %w", err)
	}
	approved := false
	for _, target := range response.Targets {
		if target.ID == provider.StrictDispatch.BindingID && target.NodeID == provider.StrictDispatch.NodeID && target.State == "active" {
			approved = true
			break
		}
	}
	if !approved {
		return errors.New("Plane Node binding is still pending or no longer belongs to this owner")
	}
	strict := *provider.StrictDispatch
	strict.Enabled = true
	if err := r.writePlaneStrictProviderConfig(loaded, providerIndex, strict); err != nil {
		return err
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providerId": provider.ID, "enabled": true, "restartRequired": true})
	}
	printSection(cmd.OutOrStdout(), "Plane strict dispatch enabled", [][2]any{{"providerId", provider.ID}, {"nodeId", strict.NodeID}, {"restartRequired", true}, {"next", "Run `looper daemon restart`."}})
	return nil
}

func (r *commandRuntime) planeApprove(cmd *cobra.Command, args []string) error {
	bindingID := strings.TrimSpace(args[0])
	if _, err := parsePlaneProtocolUUID(bindingID); err != nil {
		return fmt.Errorf("binding id: %w", err)
	}
	providerArgs := []string(nil)
	if len(args) == 2 {
		providerArgs = []string{args[1]}
	}
	_, _, provider, token, strictBaseURL, err := r.planeCommandContext(providerArgs, "")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"allowed_roles":       []string{"planner", "worker"},
		"allow_offline_queue": getBoolFlag(cmd, "allow-offline-queue"),
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf(
		"/api/workspaces/%s/projects/%s/looper/bindings/%s/approve/",
		url.PathEscape(*provider.Workspace), url.PathEscape(*provider.ProjectID), url.PathEscape(bindingID),
	)
	var binding planeLinkBinding
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPost, path, "application/json", body, &binding); err != nil {
		return fmt.Errorf("approve Plane Node binding: %w", err)
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providerId": provider.ID, "bindingId": binding.ID, "state": binding.State})
	}
	printSection(cmd.OutOrStdout(), "Plane Node binding approved", [][2]any{
		{"providerId", provider.ID}, {"bindingId", binding.ID}, {"state", binding.State},
		{"next", "Ask the binding owner to run `looper plane enable " + provider.ID + "`."},
	})
	return nil
}

func (r *commandRuntime) planeSetup(cmd *cobra.Command, args []string) error {
	for index, value := range args[:3] {
		if _, err := parsePlaneProtocolUUID(value); err != nil {
			return fmt.Errorf("role member id %d: %w", index+1, err)
		}
	}
	providerArgs := []string(nil)
	if len(args) == 4 {
		providerArgs = []string{args[3]}
	}
	_, _, provider, token, strictBaseURL, err := r.planeCommandContext(providerArgs, "")
	if err != nil {
		return err
	}
	checklistRevision, err := strconv.Atoi(strings.TrimSpace(getStringFlag(cmd, "checklist-revision")))
	if err != nil || checklistRevision <= 0 {
		return errors.New("--checklist-revision must be a positive signed rollout revision")
	}
	basePath := fmt.Sprintf("/api/workspaces/%s/projects/%s/looper", url.PathEscape(*provider.Workspace), url.PathEscape(*provider.ProjectID))
	roleBody, _ := json.Marshal(map[string]any{
		"product_member_id": args[0], "design_member_id": args[1], "qa_member_id": args[2],
	})
	var policyResponse map[string]any
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPut, basePath+"/role-policy/", "application/json", roleBody, &policyResponse); err != nil {
		return fmt.Errorf("configure Plane Looper role policy: %w", err)
	}
	activationBody, _ := json.Marshal(map[string]any{
		"action": "activate", "activation_checklist_revision": checklistRevision,
		"effective_legacy_trigger_label_ids": []string{},
	})
	var integrationResponse map[string]any
	if err := r.planeRawRequest(cmd.Context(), strictBaseURL, token, http.MethodPut, basePath+"/integration/", "application/json", activationBody, &integrationResponse); err != nil {
		return fmt.Errorf("activate Plane Looper integration: %w", err)
	}
	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"providerId": provider.ID, "policy": policyResponse["policy"], "integration": integrationResponse["integration"]})
	}
	printSection(cmd.OutOrStdout(), "Plane Looper project activated", [][2]any{
		{"providerId", provider.ID}, {"productMemberId", args[0]}, {"designMemberId", args[1]}, {"engineeringRule", "dispatch_owner"}, {"qaMemberId", args[2]}, {"checklistRevision", checklistRevision},
		{"next", "Each owner opens a Plane work item and chooses Connect my Looper."},
	})
	return nil
}

func (r *commandRuntime) planeCommandContext(args []string, strictBaseURLFlag string) (config.LoadedFileConfig, int, config.ProviderConfig, string, string, error) {
	loaded, err := r.loadConfig()
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", err
	}
	wanted := ""
	if len(args) == 1 {
		wanted = strings.TrimSpace(args[0])
	}
	index := -1
	for candidateIndex, provider := range loaded.Config.Providers {
		if provider.Kind != config.ProviderKindPlane || (wanted != "" && provider.ID != wanted) {
			continue
		}
		if index != -1 && wanted == "" {
			return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", errors.New("multiple Plane providers exist; pass a provider id")
		}
		index = candidateIndex
	}
	if index == -1 {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", errors.New("Plane provider was not found")
	}
	provider := loaded.Config.Providers[index]
	if provider.Workspace == nil || provider.ProjectID == nil || provider.TokenEnv == nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", errors.New("Plane provider requires workspace, projectId, and tokenEnv")
	}
	token := strings.TrimSpace(os.Getenv(*provider.TokenEnv))
	if token == "" {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", fmt.Errorf("Plane API token environment variable %s is empty", *provider.TokenEnv)
	}
	strictBaseURL := strings.TrimSpace(strictBaseURLFlag)
	if strictBaseURL == "" && provider.StrictDispatch != nil {
		strictBaseURL = strings.TrimSpace(provider.StrictDispatch.BaseURL)
	}
	if strictBaseURL == "" {
		strictBaseURL, err = planeWebOrigin(provider.BaseURL)
		if err != nil {
			return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", err
		}
	}
	strictBaseURL, err = planeWebOrigin(strictBaseURL)
	if err != nil {
		return config.LoadedFileConfig{}, 0, config.ProviderConfig{}, "", "", err
	}
	return loaded, index, provider, token, strings.TrimRight(strictBaseURL, "/"), nil
}

func (r *commandRuntime) planeJSONRequest(ctx context.Context, baseURL, token, method, path, contentType string, body []byte, output any) error {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/"))
	if err != nil {
		return err
	}
	return r.planeRawRequest(ctx, parsed.Scheme+"://"+parsed.Host, token, method, parsed.EscapedPath(), contentType, body, output)
}

func (r *commandRuntime) planeRawRequest(ctx context.Context, baseURL, token, method, path, contentType string, body []byte, output any) error {
	return r.planeRawRequestWithHeaders(ctx, baseURL, token, method, path, contentType, body, nil, output)
}

func (r *commandRuntime) planeRawRequestWithHeaders(ctx context.Context, baseURL, token, method, path, contentType string, body []byte, headers map[string]string, output any) error {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if token != "" {
		request.Header.Set("X-API-Key", token)
	}
	request.Header.Set("User-Agent", "looper-plane-link/1.0")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	httpClient := *r.httpClient()
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("Plane response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &failure)
		code := strings.TrimSpace(failure.Error)
		detail := strings.TrimSpace(failure.Detail)
		return &planeAPIError{Status: response.StatusCode, Code: code, Detail: detail}
	}
	if output != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, output); err != nil {
			return fmt.Errorf("decode Plane response: %w", err)
		}
	}
	return nil
}

func (r *commandRuntime) writePlaneStrictProviderConfig(loaded config.LoadedFileConfig, providerIndex int, strict config.PlaneStrictDispatchConfig) error {
	partial := loaded.Partial
	providers := partial.Providers
	if providers == nil {
		materialized := make([]config.PartialProviderConfig, len(loaded.Config.Providers))
		for index, provider := range loaded.Config.Providers {
			kind := provider.Kind
			materialized[index] = config.PartialProviderConfig{
				ID: provider.ID, Kind: &kind, BaseURL: &provider.BaseURL, GHPath: provider.GHPath,
				Auth: &provider.Auth, TokenEnv: provider.TokenEnv, TeaLogin: provider.TeaLogin, TeaPath: provider.TeaPath,
				Workspace: provider.Workspace, ProjectID: provider.ProjectID, StrictDispatch: provider.StrictDispatch,
			}
		}
		providers = &materialized
	}
	wantedID := loaded.Config.Providers[providerIndex].ID
	found := false
	for index := range *providers {
		if (*providers)[index].ID == wantedID {
			(*providers)[index].StrictDispatch = &strict
			found = true
			break
		}
	}
	if !found {
		return errors.New("Plane provider is not writable in the active config")
	}
	partial.Providers = providers
	raw, err := config.MarshalConfigFile(loaded.Metadata.ConfigPath, partial)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(loaded.Metadata.ConfigPath), 0o755); err != nil {
		return err
	}
	tmp := loaded.Metadata.ConfigPath + ".tmp"
	if err := r.writeFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return renameFile(tmp, loaded.Metadata.ConfigPath)
}

func planeWebOrigin(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("Plane URL must be an absolute URL without credentials")
	}
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" {
		return "", errors.New("Plane URL must use HTTPS outside localhost")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/api/v1")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func parsePlaneProtocolUUID(value string) (planeprotocol.UUID, error) {
	var result planeprotocol.UUID
	clean := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	decoded, err := hex.DecodeString(clean)
	if err != nil || len(decoded) != len(result) {
		return result, errors.New("invalid UUID")
	}
	copy(result[:], decoded)
	return result, nil
}

func safeFileComponent(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
	}
	if builder.Len() == 0 {
		return "plane"
	}
	return builder.String()
}
