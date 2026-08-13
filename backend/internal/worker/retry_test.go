package worker

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// The retry policy used to live in the queue: SQS redrove a message to the DLQ
// once its receive count passed maxReceiveCount, and the runner only had to
// choose between deleting the message and leaving it alone. Both halves are the
// runner's job now, so both are pinned here.
func TestRetryActionFor(t *testing.T) {
	const maxDeliveries = 3

	transient := errors.New("dial tcp: connection refused")
	permanent := Permanent("unsupported codec", errors.New("prores"))
	wrapped := fmt.Errorf("extract audio: %w", Permanent("no audio stream", nil))

	tests := []struct {
		name         string
		err          error
		receiveCount int
		want         retryAction
	}{
		{
			name:         "transient on the first delivery retries",
			err:          transient,
			receiveCount: 1,
			want:         retryLater,
		},
		{
			name:         "transient with budget left retries",
			err:          transient,
			receiveCount: 2,
			want:         retryLater,
		},
		{
			// Not retryLater. A fourth delivery would be rejected on sight by
			// the budget check in handle, so nacking here would cost a
			// redelivery and replace the failure worth reading off the DLQ with
			// a generic "budget exceeded".
			name:         "transient on the last delivery of the budget gives up",
			err:          transient,
			receiveCount: maxDeliveries,
			want:         giveUp,
		},
		{
			// The whole reason PermanentError exists. Retrying a file whose
			// codec the pipeline rejects burns the entire budget to reach the
			// same DLQ with the same message.
			name:         "permanent gives up on the first delivery",
			err:          permanent,
			receiveCount: 1,
			want:         giveUp,
		},
		{
			name:         "permanent stays permanent once wrapped",
			err:          wrapped,
			receiveCount: 1,
			want:         giveUp,
		},
		{
			// A message that somehow came back past its budget must not be
			// processed again whatever the error was.
			name:         "transient past the budget gives up",
			err:          transient,
			receiveCount: maxDeliveries + 5,
			want:         giveUp,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := retryActionFor(tc.err, tc.receiveCount, maxDeliveries)
			if got != tc.want {
				t.Errorf("retryActionFor(%v, count=%d, max=%d) = %v, want %v",
					tc.err, tc.receiveCount, maxDeliveries, got, tc.want)
			}
		})
	}
}

// The backoff has to grow, or a dependency that is down is hammered once per
// delivery until the budget runs out and the job fails for a reason that was
// only ever going to be temporary. It also has to stop growing, or a long
// backoff outlives the interest anyone has in the job.
func TestNackBackoff(t *testing.T) {
	tests := []struct {
		receiveCount int
		want         time.Duration
	}{
		{receiveCount: 1, want: initialNackBackoff},
		{receiveCount: 2, want: 2 * initialNackBackoff},
		{receiveCount: 3, want: 4 * initialNackBackoff},
		{receiveCount: 20, want: maxNackBackoff},
		// A count the broker cannot produce; it must still return something
		// usable rather than zero, which would be an immediate redelivery.
		{receiveCount: 0, want: initialNackBackoff},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("delivery %d", tc.receiveCount), func(t *testing.T) {
			got := nackBackoff(tc.receiveCount)
			if got != tc.want {
				t.Errorf("nackBackoff(%d) = %s, want %s", tc.receiveCount, got, tc.want)
			}
		})
	}
}

func TestNackBackoffNeverExceedsTheCeiling(t *testing.T) {
	for count := 0; count < 64; count++ {
		got := nackBackoff(count)
		if got < initialNackBackoff || got > maxNackBackoff {
			t.Fatalf("nackBackoff(%d) = %s, outside [%s, %s]",
				count, got, initialNackBackoff, maxNackBackoff)
		}
	}
}
