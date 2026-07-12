package durablestate

import (
	"math"
	"math/rand/v2"
	"time"
)

// RestartBackoff computes jittered exponential backoff for repeated startup failures.
// After StableAfter of healthy uptime, callers should Reset.
type RestartBackoff struct {
	Attempt     int
	Base        time.Duration
	Max         time.Duration
	LastFailure time.Time
	StableAfter time.Duration
}

// DefaultRestartBackoff returns Railway-friendly defaults.
func DefaultRestartBackoff() RestartBackoff {
	return RestartBackoff{
		Base:        500 * time.Millisecond,
		Max:         30 * time.Second,
		StableAfter: 2 * time.Minute,
	}
}

// Next returns the delay before the next startup attempt and increments Attempt.
func (b *RestartBackoff) Next() time.Duration {
	if b.Base <= 0 {
		b.Base = 500 * time.Millisecond
	}
	if b.Max <= 0 {
		b.Max = 30 * time.Second
	}
	exp := float64(b.Base) * math.Pow(2, float64(b.Attempt))
	if exp > float64(b.Max) {
		exp = float64(b.Max)
	}
	b.Attempt++
	b.LastFailure = time.Now().UTC()
	// Full jitter in [0.5, 1.0] * exp.
	jitter := 0.5 + rand.Float64()*0.5
	return time.Duration(exp * jitter)
}

// Reset clears failure streak after a stable healthy runtime window.
func (b *RestartBackoff) Reset() {
	b.Attempt = 0
	b.LastFailure = time.Time{}
}

// ShouldReset reports whether elapsed healthy runtime clears the backoff.
func (b *RestartBackoff) ShouldReset(healthyFor time.Duration) bool {
	stable := b.StableAfter
	if stable <= 0 {
		stable = 2 * time.Minute
	}
	return healthyFor >= stable
}
