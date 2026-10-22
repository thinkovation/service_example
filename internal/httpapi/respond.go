package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/thinkovation/service_example/internal/customer"
	"github.com/thinkovation/service_example/internal/store"
)

// Error codes returned in the "error.code" field. They are stable: clients may
// branch on them, unlike the human-readable message.
const (
	codeValidationFailed = "validation_failed"
	codeInvalidJSON      = "invalid_json"
	codeInvalidID        = "invalid_id"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeUnsupportedMedia = "unsupported_media_type"
	codePayloadTooLarge  = "payload_too_large"
	codeConflict         = "conflict"
	codeTimeout          = "request_timeout"
	codeInternal         = "internal_error"
)

// errorResponse is the single error shape used by every endpoint.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string                `json:"code"`
	Message   string                `json:"message"`
	Details   []customer.FieldError `json:"details,omitempty"`
	RequestID string                `json:"request_id,omitempty"`
}

// apiError is an error carrying the HTTP status and client-facing code it
// should be rendered as.
type apiError struct {
	status  int
	code    string
	message string
	details []customer.FieldError
	cause   error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.message, e.cause)
	}
	return e.message
}

func (e *apiError) Unwrap() error { return e.cause }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{status: status, code: code, message: message}
}

func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if status == http.StatusNoContent || body == nil {
		w.WriteHeader(status)
		return
	}

	payload, err := json.Marshal(body)
	if err != nil {
		loggerFrom(r.Context()).ErrorContext(r.Context(), "encode response body", slog.Any("error", err))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"failed to encode response"}}`))
		return
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(payload); err != nil {
		loggerFrom(r.Context()).DebugContext(r.Context(), "write response body", slog.Any("error", err))
	}
}

// writeError renders err as a JSON error envelope. Anything that is not an
// *apiError is treated as an unexpected failure: logged in full, reported as a
// bare 500 so internals do not leak.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	apiErr := asAPIError(ctx, err)

	if apiErr.status >= http.StatusInternalServerError {
		loggerFrom(ctx).ErrorContext(ctx, "request failed",
			slog.String("code", apiErr.code), slog.Any("error", err))
	} else {
		loggerFrom(ctx).DebugContext(ctx, "request rejected",
			slog.Int("status", apiErr.status), slog.String("code", apiErr.code),
			slog.String("reason", apiErr.message))
	}

	writeJSON(w, r, apiErr.status, errorResponse{Error: errorDetail{
		Code:      apiErr.code,
		Message:   apiErr.message,
		Details:   apiErr.details,
		RequestID: requestIDFrom(ctx),
	}})
}

// asAPIError maps domain and infrastructure errors onto their HTTP rendering.
func asAPIError(ctx context.Context, err error) *apiError {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr
	}

	var validationErr *customer.ValidationError
	if errors.As(err, &validationErr) {
		e := newAPIError(http.StatusBadRequest, codeValidationFailed, "one or more fields are invalid")
		e.details = validationErr.Fields
		return e
	}

	switch {
	case errors.Is(err, store.ErrNotFound):
		return newAPIError(http.StatusNotFound, codeNotFound, "customer not found")
	case errors.Is(err, store.ErrConflict):
		return newAPIError(http.StatusConflict, codeConflict, "customer conflicts with an existing record")
	case errors.Is(err, context.DeadlineExceeded):
		return newAPIError(http.StatusGatewayTimeout, codeTimeout, "the request took too long to process")
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// The client went away; the status is never delivered but keep it honest.
		return newAPIError(499, codeTimeout, "client closed the request")
	default:
		e := newAPIError(http.StatusInternalServerError, codeInternal, "an unexpected error occurred")
		e.cause = err
		return e
	}
}

// decodeJSON reads a JSON request body into dst. An empty body is reported as
// errEmptyBody so callers can decide whether that is acceptable.
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err, maxBytes)
	}
	// A JSON body must be a single value; anything after it is a client bug.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return newAPIError(http.StatusBadRequest, codeInvalidJSON, "body must contain a single JSON object")
	}
	return nil
}

// errEmptyBody reports a request with no body at all.
var errEmptyBody = errors.New("empty request body")

func decodeError(err error, maxBytes int64) error {
	var (
		syntaxErr      *json.SyntaxError
		typeErr        *json.UnmarshalTypeError
		maxBytesErr    *http.MaxBytesError
		invalidUnmarsh *json.InvalidUnmarshalError
	)
	switch {
	case errors.Is(err, io.EOF):
		return errEmptyBody
	case errors.As(err, &maxBytesErr):
		return newAPIError(http.StatusRequestEntityTooLarge, codePayloadTooLarge,
			fmt.Sprintf("request body must not exceed %d bytes", maxBytes))
	case errors.As(err, &syntaxErr):
		return newAPIError(http.StatusBadRequest, codeInvalidJSON,
			fmt.Sprintf("malformed JSON at byte offset %d", syntaxErr.Offset))
	case errors.As(err, &typeErr):
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return newAPIError(http.StatusBadRequest, codeInvalidJSON,
			fmt.Sprintf("field %q must be of type %s", field, typeErr.Type))
	case errors.Is(err, io.ErrUnexpectedEOF):
		return newAPIError(http.StatusBadRequest, codeInvalidJSON, "malformed JSON: unexpected end of body")
	case errors.As(err, &invalidUnmarsh):
		// A programming error on our side, not the client's.
		return fmt.Errorf("decode into invalid target: %w", err)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return newAPIError(http.StatusBadRequest, codeInvalidJSON,
			fmt.Sprintf("unknown field %s", field))
	default:
		return newAPIError(http.StatusBadRequest, codeInvalidJSON, "request body is not valid JSON")
	}
}

func requireJSONContentType(r *http.Request) error {
	raw := r.Header.Get("Content-Type")
	if raw == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return newAPIError(http.StatusUnsupportedMediaType, codeUnsupportedMedia,
			fmt.Sprintf("could not parse Content-Type %q", raw))
	}
	if mediaType != "application/json" {
		return newAPIError(http.StatusUnsupportedMediaType, codeUnsupportedMedia,
			fmt.Sprintf("Content-Type must be application/json, got %q", mediaType))
	}
	return nil
}
