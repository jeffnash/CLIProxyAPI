package pluginhost

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type streamBridge struct {
	next    atomic.Uint64
	mu      sync.Mutex
	streams map[string]*streamBridgeEntry
}

type streamBridgeEntry struct {
	mu     sync.Mutex
	chunks chan pluginapi.ExecutorStreamChunk
	closed bool
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func newStreamBridge() *streamBridge {
	return &streamBridge{streams: make(map[string]*streamBridgeEntry)}
}

func (b *streamBridge) open(ctx context.Context) (string, <-chan pluginapi.ExecutorStreamChunk, func()) {
	if b == nil {
		chunks := make(chan pluginapi.ExecutorStreamChunk)
		close(chunks)
		return "", chunks, func() {}
	}
	id := strconv.FormatUint(b.next.Add(1), 10)
	entry := &streamBridgeEntry{chunks: make(chan pluginapi.ExecutorStreamChunk, 16)}
	b.mu.Lock()
	b.streams[id] = entry
	b.mu.Unlock()
	cleanup := func() {
		b.close(id, "")
	}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			b.close(id, ctx.Err().Error())
		}()
	}
	return id, entry.chunks, cleanup
}

func (b *streamBridge) emit(ctx context.Context, id string, chunk pluginapi.ExecutorStreamChunk) error {
	if b == nil || id == "" {
		return fmt.Errorf("stream id is required")
	}
	b.mu.Lock()
	entry := b.streams[id]
	b.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("stream %s is not open", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed {
		return fmt.Errorf("stream %s is not open", id)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case entry.chunks <- chunk:
		return nil
	}
}

func (b *streamBridge) close(id string, errorMessage string) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	entry := b.streams[id]
	delete(b.streams, id)
	b.mu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed {
		return
	}
	if errorMessage != "" {
		select {
		case entry.chunks <- pluginapi.ExecutorStreamChunk{Err: fmt.Errorf("%s", errorMessage)}:
		default:
		}
	}
	entry.closed = true
	close(entry.chunks)
}

func (b *streamBridge) closeAll(errorMessage string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	ids := make([]string, 0, len(b.streams))
	for id := range b.streams {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.close(id, errorMessage)
	}
}
