// Package mcerr defines the error taxonomy used to decide whether a failed
// delivery is retried.
//
// Classification is carried by Kind rather than by sentinel errors for two
// reasons: errors from channel implementations are routinely wrapped with
// fmt.Errorf, and Kind is persisted to a column and returned over HTTP, where a
// string is the natural representation.
//
// mcerr imports nothing from this module. Domain packages depend on it, not the
// reverse.
package mcerr

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Kind classifies an error for the retry machinery.
//
// It is a string so that it survives JSON encoding and a VARCHAR column without
// a translation table on either side.
type Kind string

const (
	// KindUnknown is the default for an unclassified error. Retryable.
	KindUnknown Kind = "unknown"

	// KindPermanent means retrying cannot succeed. The delivery is failed
	// immediately, without consuming the remaining attempt budget.
	KindPermanent Kind = "permanent"

	// KindRetryable means retry after a backoff.
	KindRetryable Kind = "retryable"

	// KindRateLimited means retry, but not before RetryAfter. Set from a
	// provider's 429 response or Retry-After header.
	KindRateLimited Kind = "rate_limited"

	// KindAborted means the attempt did not reach the provider: the process is
	// shutting down, or the caller canceled. The delivery returns to pending
	// without incrementing AttemptCount.
	KindAborted Kind = "aborted"
)

var kinds = [...]Kind{
	KindUnknown,
	KindPermanent,
	KindRetryable,
	KindRateLimited,
	KindAborted,
}

// AllKinds returns every valid Kind in a stable order.
func AllKinds() []Kind {
	out := make([]Kind, len(kinds))
	copy(out, kinds[:])
	return out
}

// Valid reports whether k is one of the defined kinds.
func (k Kind) Valid() bool {
	for _, v := range kinds {
		if k == v {
			return true
		}
	}
	return false
}

// Retryable reports whether an error of this kind should be retried.
//
// KindAborted returns false: an aborted attempt is re-queued without consuming
// an attempt, which callers distinguish by checking the Kind directly.
func (k Kind) Retryable() bool {
	switch k {
	case KindRetryable, KindRateLimited, KindUnknown:
		return true
	default:
		return false
	}
}

// ConsumesAttempt reports whether a failure of this kind counts against the
// delivery's MaxAttempts budget. False only for KindAborted.
func (k Kind) ConsumesAttempt() bool {
	return k != KindAborted
}

func (k Kind) String() string { return string(k) }

// ParseKind converts a string from storage or a request into a Kind. An empty
// string yields KindUnknown; an unrecognised value is an error rather than a
// silent fallback, so corrupted data surfaces instead of becoming a retry loop.
func ParseKind(s string) (Kind, error) {
	if s == "" {
		return KindUnknown, nil
	}
	k := Kind(s)
	if !k.Valid() {
		return KindUnknown, fmt.Errorf("mcerr: unknown error kind %q", s)
	}
	return k, nil
}

// Error is the error type returned by Sender and Store implementations and by
// the dispatcher, so that one classification travels from the layer that knows
// what failed to the layer that decides whether to retry.
//
// Error survives wrapping with fmt.Errorf and %w: From recovers the Kind by
// walking the chain.
type Error struct {
	// Kind drives the retry decision. Always set by the constructors.
	Kind Kind

	// Code is a stable, machine-readable identifier for the specific failure,
	// such as "smtp_mailbox_unavailable". It appears in HTTP responses, so
	// clients branch on it. Codes are never renamed; a changed meaning gets a
	// new code.
	Code string

	// Message is a human-readable description safe to show to an API caller. It
	// must not contain PII.
	Message string

	// Channel identifies the channel the failure came from, when known.
	Channel string

	// RetryAfter overrides computed backoff. Meaningful only with
	// KindRateLimited, where the provider supplied the delay.
	RetryAfter time.Duration

	// Provider is the upstream provider's error code or request ID, for support
	// escalation. Not for control flow: providers renumber their codes.
	Provider string

	// Op names the failing operation in "Package.Method" form, following
	// net.OpError.
	Op string

	// Err is the wrapped cause, exposed through Unwrap. Not serialised.
	Err error
}

// Error implements the error interface. The kind leads so that operators can
// filter log lines on it.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b []byte
	b = append(b, "mcerr["...)
	b = append(b, e.Kind...)
	b = append(b, ']')
	if e.Op != "" {
		b = append(b, ' ')
		b = append(b, e.Op...)
	}
	if e.Channel != "" {
		b = append(b, " channel="...)
		b = append(b, e.Channel...)
	}
	if e.Code != "" {
		b = append(b, " code="...)
		b = append(b, e.Code...)
	}
	if e.Message != "" {
		b = append(b, ": "...)
		b = append(b, e.Message...)
	}
	if e.Err != nil {
		b = append(b, ": "...)
		b = append(b, e.Err.Error()...)
	}
	return string(b)
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is reports whether target matches e. Two *Error values match when their Kind
// and Code are equal, so errors.Is works against the package sentinels as well
// as against an error that was re-classified while being wrapped.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	if e == nil || t == nil {
		return e == t
	}
	return e.Kind == t.Kind && e.Code == t.Code
}

// WithChannel returns a copy of e with Channel set, leaving the receiver
// untouched so that shared sentinels can be specialised per call site.
func (e *Error) WithChannel(ch string) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Channel = ch
	return &c
}

// WithMessage returns a copy of e with Message replaced.
func (e *Error) WithMessage(format string, args ...any) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Message = fmt.Sprintf(format, args...)
	return &c
}

// WithProvider returns a copy of e carrying the upstream provider's error
// identifier.
func (e *Error) WithProvider(provider string) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Provider = provider
	return &c
}

// Wrap returns a copy of e with Err set to cause, preserving the receiver's
// classification. Use it when a lower layer's error should be recorded as the
// cause without influencing retryability.
func (e *Error) Wrap(cause error) *Error {
	if e == nil {
		return nil
	}
	c := *e
	c.Err = cause
	return &c
}

// New builds a classified error. An invalid kind is recorded as KindUnknown.
func New(kind Kind, code, format string, args ...any) *Error {
	if !kind.Valid() {
		kind = KindUnknown
	}
	return &Error{
		Kind:    kind,
		Code:    code,
		Message: fmt.Sprintf(format, args...),
	}
}

// From extracts the *Error from an error chain, returning nil for a nil input.
// An error carrying no classification is reported as KindUnknown.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Kind: KindUnknown, Message: err.Error(), Err: err}
}

// KindOf returns the Kind of an error chain, defaulting to KindUnknown. It is
// total: any error, including nil, produces a usable Kind.
func KindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	var e *Error
	if errors.As(err, &e) && e.Kind.Valid() {
		return e.Kind
	}
	return KindUnknown
}

// RetryAfterOf returns the provider-supplied retry delay, or zero when the
// error carries no hint.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return 0
}

// IsRetryable is shorthand for KindOf(err).Retryable().
func IsRetryable(err error) bool { return KindOf(err).Retryable() }

// Permanent builds a non-retryable error.
func Permanent(code, format string, args ...any) *Error {
	return New(KindPermanent, code, format, args...)
}

// Retryable builds a retryable error.
func Retryable(code, format string, args ...any) *Error {
	return New(KindRetryable, code, format, args...)
}

// RateLimited builds a retryable error carrying the provider's backoff
// instruction. A non-positive after is discarded, degrading the error to plain
// retryable behaviour rather than scheduling an immediate retry.
func RateLimited(after time.Duration, code, format string, args ...any) *Error {
	e := New(KindRateLimited, code, format, args...)
	if after > 0 {
		e.RetryAfter = after
	}
	return e
}

// Aborted builds an error for an attempt that did not happen. The delivery is
// re-queued without consuming an attempt.
func Aborted(code, format string, args ...any) *Error {
	return New(KindAborted, code, format, args...)
}

// FromContext classifies a context error as an abort, returning nil while ctx is
// live. Use it on the error path of a Send implementation so that a canceled
// context does not consume the delivery's retry budget.
func FromContext(ctx context.Context) *Error {
	if ctx == nil {
		return nil
	}
	switch err := ctx.Err(); {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return Aborted(CodeContextDeadline, "delivery deadline exceeded").Wrap(err)
	default:
		return Aborted(CodeContextCanceled, "delivery canceled").Wrap(err)
	}
}

// Codes used by this package's sentinels. Channel implementations define their
// own following the same shape: lowercase snake_case, with a provider prefix
// where the failure is provider-specific.
const (
	CodeContextCanceled = "context_canceled"
	CodeContextDeadline = "context_deadline_exceeded"
	CodeInvalidArgument = "invalid_argument"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeInternal        = "internal"
	CodeUnavailable     = "unavailable"
)

// ErrInvalidArgument is a permanent failure raised by validation. The dispatcher
// returns it to the API caller before any message row is written.
var ErrInvalidArgument = Permanent(CodeInvalidArgument, "invalid argument")

// ErrNotFound is a permanent failure meaning the addressed entity is absent.
var ErrNotFound = Permanent(CodeNotFound, "not found")

// ErrConflict is a permanent failure meaning a uniqueness constraint was
// violated, such as a duplicate idempotency key.
var ErrConflict = Permanent(CodeConflict, "conflict")

// ErrUnavailable is a retryable failure meaning a dependency is down.
var ErrUnavailable = Retryable(CodeUnavailable, "dependency unavailable")

// ErrInternal is a retryable failure for an unexpected condition. Retryable
// because an unexpected error on a Send path is more often transient than a
// logic bug, and MaxAttempts bounds the cost of being wrong.
var ErrInternal = Retryable(CodeInternal, "internal error")

// ErrShuttingDown aborts an attempt because the process is stopping.
var ErrShuttingDown = Aborted("shutting_down", "server is shutting down")

// ErrNoAddress is a permanent failure raised when a recipient has no address
// usable by the target channel. Raised by the dispatcher; Registry.Resolve
// filters first, so a Sender never sees an unaddressable request.
var ErrNoAddress = Permanent("no_address", "recipient has no address for this channel")
