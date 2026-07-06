package executor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestKiroRetrySleepHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := sleepKiroRetryWithContext(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepKiroRetryWithContext() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("sleepKiroRetryWithContext() took %s, want immediate cancellation", elapsed)
	}
}
