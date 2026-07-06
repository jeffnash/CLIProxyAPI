package pluginhost

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestStreamBridgeCloseWithFullBufferDoesNotBlock(t *testing.T) {
	bridge := newStreamBridge()
	streamID, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()

	for i := 0; i < cap(chunks); i++ {
		if errEmit := bridge.emit(context.Background(), streamID, pluginapi.ExecutorStreamChunk{Payload: []byte("chunk")}); errEmit != nil {
			t.Fatalf("emit chunk %d: %v", i, errEmit)
		}
	}

	done := make(chan struct{})
	go func() {
		bridge.close(streamID, "closed")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream close blocked on a full chunk buffer")
	}

	for range chunks {
	}
}
