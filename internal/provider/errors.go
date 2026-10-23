package provider

import (
	"errors"
	"fmt"
	"time"
)

// Kind classifies upstream failures by how callers should react to them.
type Kind int

const (
	KindUnknown      Kind = iota
	KindNotFound          // the order does not exist upstream
	KindUnavailable       // transient failure: 5xx, connection error
	KindTimeout           // the upstream did not answer in time
	KindRateLimited       // upstream throttling; see Error.RetryAfter
	KindBadResponse       // malformed or invalid payload
	KindUnauthorized      // upstream rejected our credentials or scopes
)

func (k Kind) String() string {
	switch k {
	case KindNotFound:
		return "not_found"
	case KindUnavailable:
		return "unavailable"
	case KindTimeout:
		return "timeout"
	case KindRateLimited:
		return "rate_limited"
	case KindBadResponse:
		return "bad_response"
	case KindUnauthorized:
		return "unauthorized"
	}
	return "unknown"
}

// Error is the error type returned by Provider implementations.
type Error struct {
	Kind       Kind
	Op         string        // e.g. "shopify.GetOrder"
	StatusCode int           // upstream HTTP status, if any
	RetryAfter time.Duration // minimum delay requested by the upstream, if any
	Err        error         // underlying cause; never contains credentials
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Op, e.Kind)
	if e.StatusCode != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether repeating the request may succeed.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindUnavailable, KindTimeout, KindRateLimited:
		return true
	}
	return false
}

// KindOf returns the Kind of err, or KindUnknown if err is not an *Error.
func KindOf(err error) Kind {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return KindUnknown
}

// IsRetryable reports whether err is a retryable provider error.
func IsRetryable(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Retryable()
}

// RetryAfter returns the upstream-requested delay carried by err, if any.
func RetryAfter(err error) (time.Duration, bool) {
	var pe *Error
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		return pe.RetryAfter, true
	}
	return 0, false
}

// NotFound builds a KindNotFound error.
func NotFound(op string) *Error { return &Error{Kind: KindNotFound, Op: op} }
