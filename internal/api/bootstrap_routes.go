package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	pkgapi "github.com/nexu-io/looper/pkg/api"
)

const (
	dashboardBootstrapCodePath     = apiBasePath + "/dashboard/bootstrap/code"
	dashboardBootstrapExchangePath = apiBasePath + "/dashboard/bootstrap/exchange"
)

type bootstrapCodeResponse struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expiresAt"`
}

type bootstrapExchangeRequest struct {
	Code string `json:"code"`
}

type bootstrapExchangeResponse struct {
	Token string `json:"token"`
}

func isDashboardBootstrapExchange(path, method string) bool {
	return method == http.MethodPost && path == dashboardBootstrapExchangePath
}

func (h *Handler) handleBootstrapMint(w http.ResponseWriter, r *http.Request, requestID string) {
	if h.context.Config.Server.AuthMode != config.AuthModeLocalToken {
		h.writeErrorNoStore(w, requestID, apiError{
			code:    pkgapi.ErrorCodeRouteNotFound,
			status:  http.StatusNotFound,
			message: "Unknown route: " + dashboardBootstrapCodePath,
		})
		return
	}
	if !assertMethod(r.Method, http.MethodPost, dashboardBootstrapCodePath, w, requestID, h.writeErrorNoStore) {
		return
	}

	code, expiresAt, err := h.bootstrap.Mint(h.now())
	if err != nil {
		h.writeErrorNoStore(w, requestID, internalServerError(err))
		return
	}

	h.writeSuccessNoStore(w, requestID, bootstrapCodeResponse{
		Code:      code,
		ExpiresAt: expiresAt.UTC().Format(javaScriptISOString),
	})
}

func (h *Handler) handleBootstrapExchange(w http.ResponseWriter, r *http.Request, requestID string) {
	if h.context.Config.Server.AuthMode != config.AuthModeLocalToken {
		h.writeErrorNoStore(w, requestID, apiError{
			code:    pkgapi.ErrorCodeRouteNotFound,
			status:  http.StatusNotFound,
			message: "Unknown route: " + dashboardBootstrapExchangePath,
		})
		return
	}
	if !assertMethod(r.Method, http.MethodPost, dashboardBootstrapExchangePath, w, requestID, h.writeErrorNoStore) {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, bootstrapBodyMaxSize+1))
	if err != nil {
		h.writeErrorNoStore(w, requestID, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: bootstrapInvalidMsg,
		})
		return
	}
	if len(body) > bootstrapBodyMaxSize {
		h.writeErrorNoStore(w, requestID, apiError{
			code:    pkgapi.ErrorCodeValidationFailed,
			status:  http.StatusBadRequest,
			message: bootstrapInvalidMsg,
		})
		return
	}

	var req bootstrapExchangeRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			h.writeErrorNoStore(w, requestID, apiError{
				code:    pkgapi.ErrorCodeValidationFailed,
				status:  http.StatusBadRequest,
				message: bootstrapInvalidMsg,
			})
			return
		}
	}

	if err := h.bootstrap.Exchange(req.Code, h.now()); err != nil {
		h.writeErrorNoStore(w, requestID, apiError{
			code:    pkgapi.ErrorCodeUnauthorized,
			status:  http.StatusUnauthorized,
			message: bootstrapInvalidMsg,
		})
		return
	}

	token := ""
	if h.context.Config.Server.LocalToken != nil {
		token = strings.TrimSpace(*h.context.Config.Server.LocalToken)
	}
	if token == "" {
		h.writeErrorNoStore(w, requestID, apiError{
			code:    pkgapi.ErrorCodeAuthMisconfigured,
			status:  http.StatusInternalServerError,
			message: "Local token auth is enabled but no token is configured",
		})
		return
	}

	h.writeSuccessNoStore(w, requestID, bootstrapExchangeResponse{Token: token})
}

func (h *Handler) writeSuccessNoStore(w http.ResponseWriter, requestID string, data any) {
	w.Header().Set("Cache-Control", "no-store")
	h.writeSuccess(w, requestID, data)
}

func (h *Handler) writeErrorNoStore(w http.ResponseWriter, requestID string, err apiError) {
	w.Header().Set("Cache-Control", "no-store")
	h.writeError(w, requestID, err)
}
