package vultr

import (
	"errors"
	"fmt"

	"github.com/ingvarch/tent/internal/engine"
)

// markRetryable marks err retryable for the engine when the same call may succeed later: a rate limit, after the
// wait that the answer asked for; an object in use; and a call without an answer or with a 5xx when the call is
// idempotent, so that sending it again cannot create a second object. It returns any other error, and nil, as it is.
// A create is not idempotent: after ErrUnavailable its task first searches for the object by its operation id.
func markRetryable(err error, idempotent bool) error {
	switch {
	case errors.Is(err, ErrRateLimited):
		var e *APIError
		if errors.As(err, &e) {
			return engine.Retryable(err, e.RetryAfter)
		}
		return engine.Retryable(err, 0)
	case errors.Is(err, ErrInUse), idempotent && errors.Is(err, ErrUnavailable):
		return engine.Retryable(err, 0)
	}
	return err
}

// limitHint tells how to raise a limit of the account.
const limitHint = "an account limit can be raised in the Vultr console under Billing, Limits"

// objectLimitError is an ErrLimitReached error about a limit that Vultr does not raise on request, such as 5 VPCs per
// region. It prints what tent knows of the limit, then Vultr's answer, which states whether the call reached it.
type objectLimitError struct {
	limit string // such as "ams may already have 5 VPCs, the most Vultr allows in a region"
	err   error  // Vultr's answer
}

func (e *objectLimitError) Error() string { return e.limit + ": " + e.err.Error() }
func (e *objectLimitError) Unwrap() error { return e.err }

// withLimitHint adds to an ErrLimitReached error that an account limit can be raised, and returns any other error,
// an *objectLimitError, and nil, as it is. errors.Is and errors.As see err through the result.
func withLimitHint(err error) error {
	var objectLimit *objectLimitError
	if !errors.Is(err, ErrLimitReached) || errors.As(err, &objectLimit) {
		return err
	}
	return fmt.Errorf("%w (%s)", err, limitHint)
}
