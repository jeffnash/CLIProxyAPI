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

// TestRunConnStreaming verifies the protocol-v2 path: for a request frame
// stamped proto 2, the client sends a head frame as soon as the local
// service produces headers, then chunk frames as the body arrives, then an
// end frame — instead of one buffered frame at the end.
func TestRunConnStreaming(t *testing.T) {
	var handlerDoneNano atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// NB: the done timestamp must be taken inside a deferred closure —
		// defer x.Store(time.Now()...) would evaluate Now() immediately.
		defer func() { handlerDoneNano.Store(time.Now().UnixNano()) }()
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("target ResponseWriter cannot flush")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{"chunk-1-", "chunk-2-", "chunk-3"} {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer target.Close()

	type timedFrame struct {
		at    time.Time
		frame responseFrame
	}
	frames := make(chan timedFrame, 16)
	upgrader := websocket.Upgrader{}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		frame, err := json.Marshal(requestFrame{
			ID: "req-stream", Proto: streamProtoVersion, Method: "GET", Path: "/stream",
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
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read frame: %v", err)
				return
			}
			var resp responseFrame
			if err := json.Unmarshal(payload, &resp); err != nil {
				t.Errorf("unmarshal frame: %v", err)
				return
			}
			frames <- timedFrame{at: time.Now(), frame: resp}
			if resp.Type == "end" || resp.Error != "" {
				return
			}
		}
	}))
	defer worker.Close()

	wsURL := "ws" + strings.TrimPrefix(worker.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial worker: %v", err)
	}
	client := &tunnelClient{target: target.URL, pingInterval: 0, httpClient: &http.Client{}}
	go client.runConn(0, conn)
	defer func() { _ = conn.Close() }()

	var got []timedFrame
	timeout := time.After(5 * time.Second)
loop:
	for {
		select {
		case tf := <-frames:
			got = append(got, tf)
			if tf.frame.Type == "end" {
				break loop
			}
		case <-timeout:
			t.Fatalf("timed out waiting for end frame; got %d frames: %+v", len(got), got)
		}
	}

	if len(got) < 3 {
		t.Fatalf("expected head + chunk(s) + end, got %d frames: %+v", len(got), got)
	}
	head := got[0]
	if head.frame.Type != "head" || head.frame.Status != http.StatusOK {
		t.Errorf("first frame = %+v, want head with status 200", head.frame)
	}
	// The head must arrive while the local handler is still producing the
	// body — that is the entire point of streaming.
	if done := handlerDoneNano.Load(); done == 0 || head.at.UnixNano() >= done {
		t.Errorf("head arrived at %v, handler done at %v: response was buffered, not streamed", head.at, time.Unix(0, done))
	}
	var body []byte
	for _, tf := range got[1 : len(got)-1] {
		if tf.frame.Type != "chunk" {
			t.Errorf("middle frame type = %q, want chunk", tf.frame.Type)
		}
		body = append(body, tf.frame.Body...)
	}
	if string(body) != "chunk-1-chunk-2-chunk-3" {
		t.Errorf("reassembled body = %q, want %q", body, "chunk-1-chunk-2-chunk-3")
	}
	// And the frames must actually be spread over the handler's runtime.
	if spread := got[len(got)-1].at.Sub(head.at); spread < 250*time.Millisecond {
		t.Errorf("head->end spread = %v, want >= 250ms (frames arrived in one burst)", spread)
	}
}

// TestRunConnCancel verifies that a Worker cancel frame aborts the
// in-flight local request: the local handler must observe its request
// context being cancelled.
func TestRunConnCancel(t *testing.T) {
	cancelled := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
	}))
	defer target.Close()

	upgrader := websocket.Upgrader{}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		frame, err := json.Marshal(requestFrame{
			ID: "req-cancel", Proto: streamProtoVersion, Method: "GET", Path: "/hang",
		})
		if err != nil {
			t.Errorf("marshal request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			t.Errorf("write request: %v", err)
			return
		}
		// Wait for the head frame, then cancel.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read head: %v", err)
			return
		}
		var head responseFrame
		if err := json.Unmarshal(payload, &head); err != nil || head.Type != "head" {
			t.Errorf("first frame = %s (err %v), want head", payload, err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"req-cancel","type":"cancel"}`)); err != nil {
			t.Errorf("write cancel: %v", err)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer worker.Close()

	wsURL := "ws" + strings.TrimPrefix(worker.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial worker: %v", err)
	}
	client := &tunnelClient{target: target.URL, pingInterval: 0, httpClient: &http.Client{}}
	go client.runConn(0, conn)
	defer func() { _ = conn.Close() }()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("local request was not cancelled by the Worker cancel frame")
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
