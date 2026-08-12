// Package worker provides the shared SQS consume loop for pipeline stages.
package worker

import (
	"errors"
	"fmt"
)

// PermanentError marks a failure that retrying cannot fix: a corrupt file, a
// disallowed codec, an unparseable message.
//
// The distinction is the whole point of the retry policy. Queues redrive to the
// DLQ after maxReceiveCount deliveries, which only helps if genuinely transient
// failures are the ones being retried. Treating a permanent failure as
// transient burns three visibility timeouts and lands in the DLQ carrying no
// information the first attempt did not already have. Treating a transient
// failure as permanent fails jobs that would have succeeded on retry.
type PermanentError struct {
	Reason string
	Err    error
}

func (e *PermanentError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Reason, e.Err)
	}
	return e.Reason
}

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err as a PermanentError.
func Permanent(reason string, err error) error {
	return &PermanentError{Reason: reason, Err: err}
}

// IsPermanent reports whether err is (or wraps) a PermanentError.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}
