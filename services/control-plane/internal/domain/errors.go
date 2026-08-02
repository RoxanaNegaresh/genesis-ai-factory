package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Every layer wraps these with %w; exactly one place (the HTTP
// error middleware) translates them into transport codes. Handlers never build
// error envelopes by hand.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrValidation   = errors.New("validation failed")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrUnavailable  = errors.New("unavailable")
	ErrBudget       = errors.New("budget exceeded")
	ErrCanceled     = errors.New("canceled")
)

// Error is a typed application error carrying a stable machine-readable code.
// The code is part of the API contract: clients switch on it, humans read the
// message.
type Error struct {
	Code    string
	Message string
	Kind    error
	Fields  map[string]string
	cause   error
}

func (e *Error) Error() string {
	var sb strings.Builder
	sb.WriteString(e.Code)
	if e.Message != "" {
		sb.WriteString(": ")
		sb.WriteString(e.Message)
	}
	if e.cause != nil {
		sb.WriteString(": ")
		sb.WriteString(e.cause.Error())
	}
	return sb.String()
}

// Unwrap exposes the sentinel kind so errors.Is works naturally:
// errors.Is(err, domain.ErrNotFound).
func (e *Error) Unwrap() error { return e.Kind }

// Cause returns the underlying error, if any.
func (e *Error) Cause() error { return e.cause }

// WithCause attaches a lower-level error for logging without leaking it to the
// client.
func (e *Error) WithCause(err error) *Error {
	clone := *e
	clone.cause = err
	return &clone
}

// WithField annotates a validation error with the offending field.
func (e *Error) WithField(name, problem string) *Error {
	clone := *e
	clone.Fields = map[string]string{}
	for k, v := range e.Fields {
		clone.Fields[k] = v
	}
	clone.Fields[name] = problem
	return &clone
}

func NotFound(resource string) *Error {
	return &Error{
		Code:    resource + "_not_found",
		Message: fmt.Sprintf("%s does not exist", strings.ReplaceAll(resource, "_", " ")),
		Kind:    ErrNotFound,
	}
}

func Conflict(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: ErrConflict}
}

func Invalid(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: ErrValidation}
}

func Unauthorized(message string) *Error {
	return &Error{Code: "unauthorized", Message: message, Kind: ErrUnauthorized}
}

func Forbidden(message string) *Error {
	return &Error{Code: "forbidden", Message: message, Kind: ErrForbidden}
}

func Unavailable(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: ErrUnavailable}
}

// ValidationError aggregates field-level problems so a client can highlight an
// entire form in one round trip instead of one field per request.
type ValidationError struct {
	Fields map[string]string
}

func NewValidation() *ValidationError {
	return &ValidationError{Fields: map[string]string{}}
}

func (v *ValidationError) Add(field, problem string) *ValidationError {
	v.Fields[field] = problem
	return v
}

func (v *ValidationError) Require(field, value, problem string) *ValidationError {
	if strings.TrimSpace(value) == "" {
		v.Fields[field] = problem
	}
	return v
}

func (v *ValidationError) OK() bool { return len(v.Fields) == 0 }

// Err converts the accumulator into an error, or nil when everything passed.
func (v *ValidationError) Err() error {
	if v.OK() {
		return nil
	}
	keys := make([]string, 0, len(v.Fields))
	for k := range v.Fields {
		keys = append(keys, k)
	}
	return &Error{
		Code:    "validation_failed",
		Message: "invalid fields: " + strings.Join(keys, ", "),
		Kind:    ErrValidation,
		Fields:  v.Fields,
	}
}
