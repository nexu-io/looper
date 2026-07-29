package cliapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/spf13/cobra"
)

const (
	// feishuLoginAPIBase is the Feishu open-platform host. Kept local to the login
	// command so the OAuth calls stay self-contained and don't reach into notify's
	// private helpers.
	feishuLoginAPIBase = "https://open.feishu.cn"
	// feishuLoginCallbackTimeout bounds how long we wait for the human to finish the
	// browser authorization before giving up, so a forgotten tab can't hang the CLI.
	feishuLoginCallbackTimeout = 5 * time.Minute
	// feishuLoginShutdownTimeout bounds the graceful shutdown of the local callback
	// server once we have the code (or are aborting).
	feishuLoginShutdownTimeout = 2 * time.Second
	// defaultFeishuLoginPort is the fixed loopback port the OAuth callback listens on.
	// It must match a whitelisted redirect URI in the Feishu app; the teammate
	// registers http://127.0.0.1:<this>/callback once. Overridable with --port.
	defaultFeishuLoginPort = "53682"
)

// login runs the Feishu authorization-code flow to capture the machine-runner's
// own Feishu open_id and persists it as the target project's owner. This exists
// so onboarding no longer forces a teammate to hunt for their open_id in the
// Feishu console before looper can @-mention them to merge an approved PR.
//
// The Feishu app itself must have web authorization (OAuth) enabled and
// http://127.0.0.1 (or http://localhost) whitelisted as a redirect URI in the
// developer console; otherwise the authorize step rejects the local callback.
func (r *commandRuntime) login(cmd *cobra.Command, args []string) error {
	_ = args
	ctx := cmd.Context()
	// Interactive guidance (authorize URL, waiting notice) goes to stderr so the
	// final machine-readable result on stdout stays clean for --json consumers.
	progress := cmd.ErrOrStderr()

	loaded, err := r.loadRawConfigForEdit()
	if err != nil {
		return err
	}

	appID, appSecret, err := resolveFeishuLoginCredentials(loaded.Config)
	if err != nil {
		return err
	}

	// Resolve the target project up front so an ambiguous/missing project fails fast,
	// before we send the human through the browser authorization dance.
	partial := loaded.Partial
	projectID, err := resolveFeishuLoginProjectID(partial, strings.TrimSpace(getStringFlag(cmd, "project")))
	if err != nil {
		return err
	}

	httpClient := r.httpClient()
	appAccessToken, err := fetchFeishuAppAccessToken(ctx, httpClient, appID, appSecret)
	if err != nil {
		return err
	}

	// Bind a FIXED loopback port for the OAuth redirect target. Feishu matches the
	// redirect_uri by host:port against the whitelist (a random port fails with error
	// 20029), so the port must be stable and whitelisted exactly as
	// http://127.0.0.1:<port>/callback in the developer console. Overridable with
	// --port when the default is already in use. The code exchange still stays on this
	// machine only.
	port := strings.TrimSpace(getStringFlag(cmd, "port"))
	if port == "" {
		port = defaultFeishuLoginPort
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return fmt.Errorf("start feishu login callback server on 127.0.0.1:%s (already in use? pass --port): %w", port, err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%s/callback", port)

	state, err := randomFeishuLoginState()
	if err != nil {
		_ = listener.Close()
		return err
	}
	authorizeURL := buildFeishuAuthorizeURL(appID, redirectURI, state)

	callbackCtx, cancel := context.WithTimeout(ctx, feishuLoginCallbackTimeout)
	defer cancel()

	// afterListening fires once the callback server is accepting connections, so we
	// only advertise the authorize URL after the redirect target is actually live.
	code, err := waitForFeishuLoginCode(callbackCtx, listener, state, func() {
		_, _ = fmt.Fprintln(progress, "Opening your browser to authorize with Feishu.")
		_, _ = fmt.Fprintf(progress, "If it does not open, visit this URL manually:\n%s\n", authorizeURL)
		if openErr := openLoginBrowser(r.platform(), authorizeURL); openErr != nil {
			_, _ = fmt.Fprintf(progress, "(could not launch a browser automatically: %v)\n", openErr)
		}
		_, _ = fmt.Fprintln(progress, "Waiting for the Feishu authorization callback...")
	})
	if err != nil {
		return err
	}

	openID, name, err := exchangeFeishuAuthCode(ctx, httpClient, appAccessToken, code)
	if err != nil {
		return err
	}

	index, err := setProjectOwnerInPartial(&partial, projectID, openID)
	if err != nil {
		return err
	}
	if err := r.writeConfigFile(loaded.Metadata.ConfigPath, partial); err != nil {
		return err
	}

	if getBoolFlag(cmd, "json") {
		return writeJSON(cmd.OutOrStdout(), map[string]any{
			"openId":       openID,
			"name":         name,
			"projectId":    projectID,
			"projectIndex": index,
			"configPath":   loaded.Metadata.ConfigPath,
			"updated":      true,
		})
	}

	displayName := name
	if strings.TrimSpace(displayName) == "" {
		displayName = "(name unavailable)"
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "已登录 %s(open_id %s) → 写入 projects[%d].owner.feishuOpenId\n", displayName, openID, index)
	return err
}

// resolveFeishuLoginCredentials pulls the Feishu app id/secret from the env vars
// named by the notifications webhook config (appIdEnv / appSecretEnv), matching
// how the notify gateway resolves them. It never stores secret values in config —
// only their env var names — so the resolution mirrors that indirection here.
func resolveFeishuLoginCredentials(cfg config.Config) (string, string, error) {
	webhook := cfg.Notifications.Webhook
	appIDEnv := strings.TrimSpace(webhook.AppIDEnv)
	appSecretEnv := strings.TrimSpace(webhook.AppSecretEnv)
	if appIDEnv == "" || appSecretEnv == "" {
		return "", "", fmt.Errorf("feishu login requires notifications.webhook.appIdEnv and notifications.webhook.appSecretEnv to be set (Feishu app mode); configure them and rerun")
	}
	appID := strings.TrimSpace(os.Getenv(appIDEnv))
	appSecret := strings.TrimSpace(os.Getenv(appSecretEnv))
	if appID == "" || appSecret == "" {
		return "", "", fmt.Errorf("feishu login requires the Feishu app credentials: export %s and %s before running `looper login`", appIDEnv, appSecretEnv)
	}
	return appID, appSecret, nil
}

// resolveFeishuLoginProjectID picks which project the captured owner open_id
// belongs to. With a single configured project the choice is unambiguous; with
// several, the caller must disambiguate via --project so we never guess an owner
// onto the wrong project. It only reads config; the write happens after the OAuth
// exchange succeeds.
func resolveFeishuLoginProjectID(partial config.PartialConfig, projectFlag string) (string, error) {
	if partial.Projects == nil || len(*partial.Projects) == 0 {
		return "", fmt.Errorf("no projects configured; run `looper bootstrap` or `looper project add` before `looper login`")
	}
	projects := *partial.Projects
	if projectFlag != "" {
		for i := range projects {
			if projects[i].ID == projectFlag {
				return projects[i].ID, nil
			}
		}
		return "", fmt.Errorf("no project with id %q in config", projectFlag)
	}
	if len(projects) == 1 {
		return projects[0].ID, nil
	}
	ids := make([]string, 0, len(projects))
	for i := range projects {
		ids = append(ids, projects[i].ID)
	}
	return "", fmt.Errorf("config has %d projects; pass --project <id> to choose one (%s)", len(projects), strings.Join(ids, ", "))
}

// setProjectOwnerInPartial writes the captured open_id into the owner of the
// project identified by projectID, preserving every other field, and returns the
// project's index (for the "projects[N].owner..." success line). Kept pure so the
// config mutation can be unit-tested without a live Feishu round-trip.
func setProjectOwnerInPartial(partial *config.PartialConfig, projectID, openID string) (int, error) {
	if partial.Projects == nil {
		return 0, fmt.Errorf("no projects configured; run `looper bootstrap` or `looper project add` before `looper login`")
	}
	projects := *partial.Projects
	for i := range projects {
		if projects[i].ID == projectID {
			projects[i].Owner = &config.FeishuActorConfig{FeishuOpenID: openID}
			return i, nil
		}
	}
	return 0, fmt.Errorf("no project with id %q in config", projectID)
}

// buildFeishuAuthorizeURL assembles the Feishu authorization-code entry point.
// url.Values.Encode handles percent-encoding (notably the loopback redirect_uri),
// so this stays a pure, deterministic string builder that tests can assert on.
func buildFeishuAuthorizeURL(appID, redirectURI, state string) string {
	query := url.Values{}
	query.Set("app_id", appID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("state", state)
	return feishuLoginAPIBase + "/open-apis/authen/v1/authorize?" + query.Encode()
}

// randomFeishuLoginState mints an unguessable state token so we can detect a
// forged/replayed callback (CSRF) before trusting its authorization code.
func randomFeishuLoginState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate feishu login state: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// waitForFeishuLoginCode serves the loopback /callback endpoint until Feishu
// redirects back with an authorization code, then returns it. It verifies the
// state parameter to reject CSRF, shows the human a close-this-tab page, and
// tears the server down on exit. afterListening runs once the server is live so
// the caller can advertise the authorize URL only after the redirect target is
// reachable.
func waitForFeishuLoginCode(ctx context.Context, listener net.Listener, expectedState string, afterListening func()) (string, error) {
	resultCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, req *http.Request) {
		query := req.URL.Query()
		code := strings.TrimSpace(query.Get("code"))
		gotState := strings.TrimSpace(query.Get("state"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if gotState != expectedState {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, feishuLoginResultHTML(false))
			sendFeishuLoginError(errCh, fmt.Errorf("feishu login state mismatch (possible CSRF); aborting"))
			return
		}
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, feishuLoginResultHTML(false))
			sendFeishuLoginError(errCh, fmt.Errorf("feishu login callback did not include an authorization code"))
			return
		}
		_, _ = io.WriteString(w, feishuLoginResultHTML(true))
		select {
		case resultCh <- code:
		default:
		}
	})

	server := &http.Server{Handler: mux}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			sendFeishuLoginError(errCh, fmt.Errorf("feishu login callback server: %w", serveErr))
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), feishuLoginShutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if afterListening != nil {
		afterListening()
	}

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("timed out waiting for the Feishu login callback: %w", ctx.Err())
	case err := <-errCh:
		return "", err
	case code := <-resultCh:
		return code, nil
	}
}

// sendFeishuLoginError delivers the first callback error without blocking the HTTP
// handler goroutine (the channel is buffered for exactly one delivery).
func sendFeishuLoginError(errCh chan<- error, err error) {
	select {
	case errCh <- err:
	default:
	}
}

// feishuLoginResultHTML renders the minimal page shown in the browser tab after
// the redirect, telling the human they can close it (bilingual, no assets).
func feishuLoginResultHTML(success bool) string {
	body := "登录成功，可以关掉这个页面 / You can close this tab."
	if !success {
		body = "登录失败，请回到终端查看错误 / Login failed; check your terminal."
	}
	return "<!doctype html><html><head><meta charset=\"utf-8\"><title>looper login</title></head><body><p>" + body + "</p></body></html>"
}

// openLoginBrowser best-effort launches the platform browser at target. Failure is
// non-fatal: the caller has already printed the URL for manual opening.
func openLoginBrowser(platform, target string) error {
	var name string
	var args []string
	switch platform {
	case "darwin":
		name, args = "open", []string{target}
	case "windows":
		name, args = "cmd", []string{"/c", "start", "", target}
	default:
		name, args = "xdg-open", []string{target}
	}
	return exec.Command(name, args...).Start()
}

// fetchFeishuAppAccessToken exchanges the app id/secret for an app_access_token,
// which authorizes the later code→open_id exchange. Mirrors notify's tenant-token
// call but hits the app_access_token endpoint and stays self-contained.
func fetchFeishuAppAccessToken(ctx context.Context, httpClient *http.Client, appID, appSecret string) (string, error) {
	body, err := json.Marshal(map[string]string{"app_id": appID, "app_secret": appSecret})
	if err != nil {
		return "", fmt.Errorf("encode feishu app_access_token request: %w", err)
	}
	respBody, err := doFeishuLoginJSON(ctx, httpClient, feishuLoginAPIBase+"/open-apis/auth/v3/app_access_token/internal", nil, body)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Code           int    `json:"code"`
		Msg            string `json:"msg"`
		AppAccessToken string `json:"app_access_token"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode feishu app_access_token response: %w", err)
	}
	if parsed.Code != 0 || strings.TrimSpace(parsed.AppAccessToken) == "" {
		return "", fmt.Errorf("feishu app_access_token error code %d: %s", parsed.Code, parsed.Msg)
	}
	return parsed.AppAccessToken, nil
}

// exchangeFeishuAuthCode trades the authorization code for the authenticated
// user's identity, returning their open_id and display name. The app_access_token
// authorizes the request as this Feishu app.
func exchangeFeishuAuthCode(ctx context.Context, httpClient *http.Client, appAccessToken, code string) (string, string, error) {
	body, err := json.Marshal(map[string]string{"grant_type": "authorization_code", "code": code})
	if err != nil {
		return "", "", fmt.Errorf("encode feishu access_token request: %w", err)
	}
	respBody, err := doFeishuLoginJSON(ctx, httpClient, feishuLoginAPIBase+"/open-apis/authen/v1/access_token", map[string]string{
		"Authorization": "Bearer " + appAccessToken,
	}, body)
	if err != nil {
		return "", "", err
	}
	return parseFeishuAccessTokenResponse(respBody)
}

// parseFeishuAccessTokenResponse extracts data.open_id and data.name from the
// Feishu access_token response. Split out from the HTTP call so the JSON contract
// can be unit-tested against fixture bodies.
func parseFeishuAccessTokenResponse(body []byte) (string, string, error) {
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OpenID string `json:"open_id"`
			Name   string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("decode feishu access_token response: %w", err)
	}
	if parsed.Code != 0 {
		return "", "", fmt.Errorf("feishu access_token error code %d: %s", parsed.Code, parsed.Msg)
	}
	openID := strings.TrimSpace(parsed.Data.OpenID)
	if openID == "" {
		return "", "", fmt.Errorf("feishu access_token response missing data.open_id")
	}
	return openID, strings.TrimSpace(parsed.Data.Name), nil
}

// doFeishuLoginJSON POSTs a JSON body to a Feishu endpoint and returns the raw
// response body. Kept tiny and local (rather than reusing notify's private
// helper) so the login command has no cross-package coupling.
func doFeishuLoginJSON(ctx context.Context, httpClient *http.Client, endpoint string, headers map[string]string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("build feishu request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call feishu: %w", err)
	}
	defer response.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read feishu response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("feishu responded with status %d", response.StatusCode)
	}
	return respBody, nil
}
