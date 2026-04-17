package api

type ErrorCode string

const (
	ErrorCodeActiveRunNotFound          ErrorCode = "ACTIVE_RUN_NOT_FOUND"
	ErrorCodeAgentNotConfigured         ErrorCode = "AGENT_NOT_CONFIGURED"
	ErrorCodeAuthMisconfigured          ErrorCode = "AUTH_MISCONFIGURED"
	ErrorCodeInternalError              ErrorCode = "INTERNAL_ERROR"
	ErrorCodeLoopConflict               ErrorCode = "LOOP_CONFLICT"
	ErrorCodeLoopNotFound               ErrorCode = "LOOP_NOT_FOUND"
	ErrorCodeMethodNotAllowed           ErrorCode = "METHOD_NOT_ALLOWED"
	ErrorCodeProjectAmbiguous           ErrorCode = "PROJECT_AMBIGUOUS"
	ErrorCodeProjectIDConflict          ErrorCode = "PROJECT_ID_CONFLICT"
	ErrorCodeProjectNotFound            ErrorCode = "PROJECT_NOT_FOUND"
	ErrorCodeProjectsUnavailable        ErrorCode = "PROJECTS_UNAVAILABLE"
	ErrorCodePRNotFound                 ErrorCode = "PR_NOT_FOUND"
	ErrorCodePullRequestNotFound        ErrorCode = "PULL_REQUEST_NOT_FOUND"
	ErrorCodePullRequestProjectMismatch ErrorCode = "PULL_REQUEST_PROJECT_MISMATCH"
	ErrorCodeRouteNotFound              ErrorCode = "ROUTE_NOT_FOUND"
	ErrorCodeRuntimeControlUnavailable  ErrorCode = "RUNTIME_CONTROL_UNAVAILABLE"
	ErrorCodeUnauthorized               ErrorCode = "UNAUTHORIZED"
	ErrorCodeValidationFailed           ErrorCode = "VALIDATION_FAILED"
)

type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Details any       `json:"details,omitempty"`
}

type Envelope[T any] struct {
	OK        bool   `json:"ok"`
	Data      *T     `json:"data,omitempty"`
	Error     *Error `json:"error,omitempty"`
	RequestID string `json:"requestId"`
}

func Success[T any](requestID string, data T) Envelope[T] {
	return Envelope[T]{
		OK:        true,
		Data:      &data,
		RequestID: requestID,
	}
}

func Failure(requestID string, code ErrorCode, message string, details any) Envelope[any] {
	return Envelope[any]{
		OK:        false,
		Error:     &Error{Code: code, Message: message, Details: details},
		RequestID: requestID,
	}
}

func (c ErrorCode) String() string {
	return string(c)
}

func (c ErrorCode) Status() int {
	switch c {
	case ErrorCodeActiveRunNotFound:
		return 404
	case ErrorCodeAgentNotConfigured:
		return 400
	case ErrorCodeAuthMisconfigured:
		return 500
	case ErrorCodeInternalError:
		return 500
	case ErrorCodeLoopConflict:
		return 409
	case ErrorCodeLoopNotFound:
		return 404
	case ErrorCodeMethodNotAllowed:
		return 405
	case ErrorCodeProjectAmbiguous:
		return 409
	case ErrorCodeProjectIDConflict:
		return 409
	case ErrorCodeProjectNotFound:
		return 404
	case ErrorCodeProjectsUnavailable:
		return 500
	case ErrorCodePRNotFound:
		return 404
	case ErrorCodePullRequestNotFound:
		return 404
	case ErrorCodePullRequestProjectMismatch:
		return 409
	case ErrorCodeRouteNotFound:
		return 404
	case ErrorCodeRuntimeControlUnavailable:
		return 501
	case ErrorCodeUnauthorized:
		return 401
	case ErrorCodeValidationFailed:
		return 400
	default:
		return 500
	}
}

func AllErrorCodes() []ErrorCode {
	return []ErrorCode{
		ErrorCodeActiveRunNotFound,
		ErrorCodeAgentNotConfigured,
		ErrorCodeAuthMisconfigured,
		ErrorCodeInternalError,
		ErrorCodeLoopConflict,
		ErrorCodeLoopNotFound,
		ErrorCodeMethodNotAllowed,
		ErrorCodeProjectsUnavailable,
		ErrorCodeProjectAmbiguous,
		ErrorCodeProjectIDConflict,
		ErrorCodeProjectNotFound,
		ErrorCodePRNotFound,
		ErrorCodePullRequestNotFound,
		ErrorCodePullRequestProjectMismatch,
		ErrorCodeRouteNotFound,
		ErrorCodeRuntimeControlUnavailable,
		ErrorCodeUnauthorized,
		ErrorCodeValidationFailed,
	}
}
