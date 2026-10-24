package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestRunConnForwardsAndPings stands up a fake Worker (WebSocket server) and
// a slow local target, then verifies the tunnel forwards the request, returns
// the whole response as one frame, and sends keepalive pings while the slow
// response is in flight.
func TestRunConnForwardsAndPings(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Stay silent long enough for several ping intervals to elapse.
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer target.Close()

	var pings int64
	upgrader := websocket.Upgrader{}
	frames := make(chan responseFrame, 1)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		conn.SetPingHandler(func(string) error {
			atomic.AddInt64(&pings, 1)
			return conn.WriteMessage(websocket.PongMessage, nil)
		})
		frame, err := json.Marshal(requestFrame{
			ID:     "req-1",
			Method: "POST",
			Path:   "/v1/chat/completions",
			Body:   []byte(`{"model":"x"}`),
		})
		if err != nil {
			t.Errorf("marshal request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			t.Errorf("write request: %v", err)
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read response: %v", err)
			return
		}
		var resp responseFrame
		if err := json.Unmarshal(payload, &resp); err != nil {
			t.Errorf("unmarshal response: %v", err)
			return
		}
		frames <- resp
		// Hold the connection open briefly so trailing pings are observed.
		time.Sleep(150 * time.Millisecond)
	}))
	defer worker.Close()

	wsURL := "ws" + strings.TrimPrefix(worker.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial worker: %v", err)
	}
	client := &tunnelClient{
		target:       target.URL,
		pingInterval: 50 * time.Millisecond,
		httpClient:   &http.Client{},
	}
	done := make(chan struct{})
	go func() {
		client.runConn(0, conn)
		close(done)
	}()

	select {
	case resp := <-frames:
		if resp.ID != "req-1" || resp.Status != http.StatusOK || string(resp.Body) != `{"ok":true}` {
			t.Errorf("unexpected response frame: %+v body=%s", resp, resp.Body)
		}
		if resp.Error != "" {
			t.Errorf("unexpected error frame: %s", resp.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for response frame")
	}

	if got := atomic.LoadInt64(&pings); got < 2 {
		t.Errorf("expected keepalive pings during slow request, got %d", got)
	}

	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runConn did not return after connection close")
	}
}

// The deployed Worker sends frame headers as name -> string
// (Object.fromEntries of the fetch Headers). A regression here breaks every
// tunneled request, so pin the exact Worker-shaped frame.
func TestRequestFrameHeadersStringValues(t *testing.T) {
	payload := []byte(`{"id":"1","method":"POST","path":"/v1/chat/completions","headers":{"Content-Type":"application/json","Authorization":"Bearer x"},"body":"e30="}`)
	var frame requestFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("unmarshal worker-shaped frame: %v", err)
	}
	if got := frame.Headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := frame.Headers.Get("Authorization"); got != "Bearer x" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer x")
	}
	if frame.ID != "1" || frame.Method != "POST" || frame.Path != "/v1/chat/completions" {
		t.Errorf("frame fields = %+v", frame)
	}
}

// The Go http.Header form (name -> []string) must keep working too.
func TestRequestFrameHeadersArrayValues(t *testing.T) {
	payload := []byte(`{"id":"2","method":"GET","path":"/v1/models","headers":{"X-Test":["a","b"]}}`)
	var frame requestFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("unmarshal go-shaped frame: %v", err)
	}
	values := frame.Headers.Values("X-Test")
	if len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Errorf("X-Test = %v, want [a b]", values)
	}
}

func TestNormalizeWorkerURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"example.workers.dev", "wss://example.workers.dev/tunnel"},
		{"example.workers.dev/", "wss://example.workers.dev/tunnel"},
		{"example.workers.dev/custom", "wss://example.workers.dev/custom"},
		{"wss://example.workers.dev/tunnel", "wss://example.workers.dev/tunnel"},
		{"ws://localhost:8787/tunnel", "ws://localhost:8787/tunnel"},
		{"  example.workers.dev  ", "wss://example.workers.dev/tunnel"},
	}
	for _, tc := range cases {
		if got := normalizeWorkerURL(tc.in); got != tc.want {
			t.Errorf("normalizeWorkerURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
