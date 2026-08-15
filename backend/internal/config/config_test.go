package config

import (
	"testing"
	"time"
)

// TestJobCacheTTLFromEnv covers the one knob whose value decides whether the app
// ever sees a pipeline stage: GET /jobs/{id} is served from a cache no worker can
// invalidate, so JOB_CACHE_TTL is the observation lag.
func TestJobCacheTTLFromEnv(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"unset uses the documented default", "", DefaultJobCacheTTL},
		{"seconds", "5s", 5 * time.Second},
		{"sub-second", "250ms", 250 * time.Millisecond},
		{"minutes", "2m", 2 * time.Minute},

		// Unusable values fall back instead of failing startup — the same rule
		// the other duration knobs follow. A typo in a tuning value must not
		// take the API down mid-demo.
		{"malformed falls back", "10 seconds", DefaultJobCacheTTL},
		{"bare number falls back", "10", DefaultJobCacheTTL},
		{"nonsense falls back", "forever", DefaultJobCacheTTL},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JOB_CACHE_TTL", tc.raw)

			if got := Load().JobCacheTTL; got != tc.want {
				t.Errorf("JOB_CACHE_TTL=%q gave JobCacheTTL = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

// TestDefaultJobCacheTTL pins the default itself. Ten seconds was the value that
// hid stage transitions behind five identical poll responses; the point of the
// change is the number, so a silent revert should fail here.
func TestDefaultJobCacheTTL(t *testing.T) {
	if DefaultJobCacheTTL != time.Second {
		t.Errorf("DefaultJobCacheTTL = %s, want 1s", DefaultJobCacheTTL)
	}
}
