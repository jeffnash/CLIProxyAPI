package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestStreamTimeoutTrackerCancelsContext(t *testing.T) {
	previousTick := streamTimeoutTrackerTick
	streamTimeoutTrackerTick = 10 * time.Millisecond
	t.Cleanup(func() { streamTimeoutTrackerTick = previousTick })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker := newStreamTimeoutTracker(&config.Config{
		Grok: config.GrokConfig{StreamTotalTimeoutSeconds: 1},
	})
	tracker.mu.Lock()
	tracker.start = time.Now().Add(-2 * time.Second)
	tracker.mu.Unlock()

	reasonCh := tracker.Start(ctx, cancel)
	select {
	case reason := <-reasonCh:
		if !strings.Contains(reason, "stream timed out") {
			t.Fatalf("reason = %q, want timeout", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for timeout reason")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not canceled")
	}
}
