package orders

import (
	"errors"
	"time"
)

// Kind is a client-facing failure category. The API layer maps each Kind to
// exactly one HTTP status.
type Kind int

const (
	KindInternal     Kind = iota
	KindInvalidID         // 400: malformed order ID
	KindNotFound          // 404: missing, or not visible to the caller
	KindUnavailable       // 503: upstream down, throttled or circuit open
	KindTimeout           // 504: upstream did not answer in time
	KindUpstream          // 502: upstream answered with something unusable
	KindCanceled          // the caller went away
	KindInvalidQuery      // 400: search pattern is not a valid regular expression
)

func (k Kind) String() string {
	switch k {
	case KindInvalidID:
		return "invalid_id"
	case KindNotFound:
		return "not_found"
	case KindUnavailable:
		return "unavailable"
	case KindTimeout:
		return "timeout"
	case KindUpstream:
		return "upstream"
	case KindCanceled:
		return "canceled"
	case KindInvalidQuery:
		return "invalid_query"
	}
	return "internal"
}

// Error is returned by Service methods.
type Error struct {
	Kind       Kind
	RetryAfter time.Duration // hint for the client; 0 when unknown
	Err        error         // internal cause, for logs only
}

func (e *Error) Error() string {
	if e.Err != nil {
		return "orders: " + e.Kind.String() + ": " + e.Err.Error()
	}
	return "orders: " + e.Kind.String()
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the Kind of err (KindInternal for foreign errors).
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}
