package api

import (
	"encoding/json"
	"net/http"
)

// Stable error codes. Clients branch on these, never on the message.
const (
	CodeInvalidArgument  = "invalid_argument"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeConflict         = "conflict"
	CodePayloadTooLarge  = "payload_too_large"
	CodeUnprocessable    = "unprocessable"
	CodeRateLimited      = "rate_limited"
	CodeInternal         = "internal"
	CodeNotImplemented   = "not_implemented"
	CodeUnavailable      = "unavailable"
)

// Meta is the envelope's non-payload section.
type Meta struct {
	RequestID  string `json:"request_id,omitempty"`
	Count      int    `json:"count,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Envelope is the success shape: {"data": ..., "meta": ...}.
type Envelope struct {
	Data any   `json:"data"`
	Meta *Meta `json:"meta,omitempty"`
}

// ErrorBody is the machine-readable failure shape.
type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// ErrorEnvelope wraps ErrorBody so failures are distinguishable from data.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeData emits a success envelope. meta may be nil.
func writeData(w http.ResponseWriter, r *http.Request, data any, meta *Meta) {
	if meta == nil {
		meta = &Meta{}
	}
	if meta.RequestID == "" {
		meta.RequestID = requestIDFrom(r)
	}
	writeJSON(w, http.StatusOK, Envelope{Data: data, Meta: meta})
}

// writeError emits a failure envelope. Detail must be safe to show a caller; it
// is not a place for internal state or wrapped database errors.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	writeJSON(w, status, ErrorEnvelope{Error: ErrorBody{
		Code:      code,
		Message:   detail,
		RequestID: requestIDFrom(r),
	}})
}
