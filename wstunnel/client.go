// Command wstunnel-client exposes a local CLIProxyAPI instance through a
// Cloudflare Worker + Durable Object. The Durable Object accepts client
// HTTP requests and forwards each one as a JSON frame over a persistent
// WebSocket connection held by this client; the client calls the local
// proxy and returns the response.
//
// Frame protocol (one JSON object per text message):
//
//	request:  {"id":"...","proto":2,"method":"POST","path":"/v1/...","headers":{...},"body":"<base64>"}
//	response (protocol v1, request carried no proto):
//	          {"id":"...","status":200,"headers":{...},"body":"<base64>"}
//	response (protocol v2, streamed):
//	          {"id":"...","type":"head","status":200,"headers":{...}}
//	          {"id":"...","type":"chunk","body":"<base64>"}   (zero or more)
//	          {"id":"...","type":"end"}
//	failure:  {"id":"...","error":"..."}  (before the head)
//	          {"id":"...","type":"error","error":"..."}  (mid-stream)
//	cancel:   {"id":"...","type":"cancel"}  (Worker -> client: the caller
//	          went away; abort the local request)
//
// The per-request protocol is chosen from the request frame itself: a
// Worker that understands streaming marks its requests "proto":2 and the
// client answers with head/chunk/end frames; an older Worker sends no
// proto field and the client answers with the single v1 frame. Either
// side can therefore be deployed first.
//
// Long generations leave the socket silent for minutes while the local
// response streams in, so each connection sends WebSocket pings on an
// interval (see -ping-interval) to keep intermediaries from reaping it as
// idle. Cloudflare answers protocol pings at the platform level.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// streamProtoVersion is the newest frame protocol this client speaks. The
// Worker stamps it onto each request frame it forwards; see the package
// comment for how the per-request protocol is negotiated.
const streamProtoVersion = 2

// streamChunkSize bounds one chunk frame's raw (pre-base64) payload.
const streamChunkSize = 32 * 1024

// streamCoalesceWindow is how long streamed body bytes may accumulate
// before a partial chunk frame is flushed. Streamed responses (SSE above
// all) arrive as many tiny reads; sending one WebSocket frame per read
// multiplies the Durable Object request count, which the account's daily
// free-tier cap counts per message. Coalescing trades at most this much
// added latency on later chunks for an order-of-magnitude fewer frames.
// The first chunk is never delayed, so time-to-first-byte is unchanged.
const streamCoalesceWindow = 40 * time.Millisecond

type requestFrame struct {
	ID      string      `json:"id"`
	Proto   int         `json:"proto,omitempty"`
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

// UnmarshalJSON accepts request frames whose headers are encoded either as
// name -> []string (the Go http.Header form) or as name -> string, which is
// what the Worker sends (Object.fromEntries of the fetch Headers). Both
// shapes occur on the wire, so tolerate both here. Regression coverage lives
// in client_test.go.
func (f *requestFrame) UnmarshalJSON(data []byte) error {
	type alias requestFrame
	var raw struct {
		alias
		Headers map[string]json.RawMessage `json:"headers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*f = requestFrame(raw.alias)
	f.Headers = make(http.Header, len(raw.Headers))
	for name, rawValue := range raw.Headers {
		var list []string
		if err := json.Unmarshal(rawValue, &list); err != nil {
			var single string
			if err := json.Unmarshal(rawValue, &single); err != nil {
				continue
			}
			list = []string{single}
		}
		f.Headers[textproto.CanonicalMIMEHeaderKey(name)] = list
	}
	return nil
}

type responseFrame struct {
	ID      string      `json:"id"`
	Type    string      `json:"type,omitempty"` // "", "head", "chunk", "end", "error"
	Status  int         `json:"status,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// controlFrame is the Worker -> client envelope probed before a payload is
// treated as a request: cancel frames carry a type and no method/path.
type controlFrame struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// inflightRequests tracks the cancel func of every request being proxied
// on one connection, so a Worker cancel frame can abort exactly the local
// request it names.
type inflightRequests struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

func newInflightRequests() *inflightRequests {
	return &inflightRequests{m: make(map[string]context.CancelFunc)}
}

func (r *inflightRequests) add(id string, cancel context.CancelFunc) {
	r.mu.Lock()
	r.m[id] = cancel
	r.mu.Unlock()
}

func (r *inflightRequests) remove(id string) {
	r.mu.Lock()
	delete(r.m, id)
	r.mu.Unlock()
}

func (r *inflightRequests) cancel(id string) {
	r.mu.Lock()
	cancel, ok := r.m[id]
	r.mu.Unlock()
	if ok {
		cancel()
	}
}

type tunnelClient struct {
	workerURL    string
	target       string
	pingInterval time.Duration
	httpClient   *http.Client
	// secret, when non-empty, is sent as the X-Tunnel-Secret header on every
	// dial; the Worker refuses tunnel connections without it.
	secret string
}

// normalizeWorkerURL accepts either a full WebSocket URL
// ("wss://host/tunnel") or a bare Worker host ("host"), which expands to
// "wss://host/tunnel" — the form accepted before -worker took a verbatim
// URL. Accepting both keeps existing units and scripts working.
func normalizeWorkerURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "ws://") || strings.HasPrefix(raw, "wss://") {
		return raw
	}
	host := strings.TrimSuffix(raw, "/")
	if !strings.Contains(host, "/") {
		host += "/tunnel"
	}
	return "wss://" + host
}

func main() {
	worker := flag.String("worker", "", "Worker WebSocket URL (wss://...) or bare Worker host")
	target := flag.String("target", "http://127.0.0.1:8317", "local proxy base URL")
	conns := flag.Int("conns", 4, "number of persistent tunnel connections")
	pingInterval := flag.Duration("ping-interval", 20*time.Second, "WebSocket keepalive ping interval (0 disables)")
	secretFile := flag.String("secret-file", "", "file containing the tunnel admission secret, sent as X-Tunnel-Secret on dial")
	flag.Parse()

	if strings.TrimSpace(*worker) == "" {
		fmt.Fprintln(os.Stderr, "wstunnel-client: -worker is required")
		os.Exit(2)
	}
	if *conns < 1 {
		fmt.Fprintln(os.Stderr, "wstunnel-client: -conns must be >= 1")
		os.Exit(2)
	}

	secret := ""
	if strings.TrimSpace(*secretFile) != "" {
		raw, err := os.ReadFile(*secretFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wstunnel-client: read secret file: %v\n", err)
			os.Exit(2)
		}
		secret = strings.TrimSpace(string(raw))
		if secret == "" {
			fmt.Fprintln(os.Stderr, "wstunnel-client: secret file is empty")
			os.Exit(2)
		}
	}

	workerURL := normalizeWorkerURL(*worker)
	client := &tunnelClient{
		workerURL:    workerURL,
		target:       strings.TrimSuffix(*target, "/"),
		pingInterval: *pingInterval,
		secret:       secret,
		// No timeout: proxied generations legitimately run for minutes.
		httpClient: &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	log.Printf("wstunnel-client: %d connection(s) to %s, target %s, ping %s", *conns, workerURL, client.target, *pingInterval)
	var wg sync.WaitGroup
	for i := 0; i < *conns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client.runConnLoop(id)
		}(i)
	}
	wg.Wait()
}

// runConnLoop keeps one tunnel connection alive, redialing with backoff
// whenever it drops.
func (c *tunnelClient) runConnLoop(id int) {
	backoff := time.Second
	for {
		var headers http.Header
		if c.secret != "" {
			headers = http.Header{"X-Tunnel-Secret": []string{c.secret}}
		}
		conn, _, err := websocket.DefaultDialer.Dial(c.workerURL, headers)
		if err != nil {
			log.Printf("conn %d: dial: %v (retry in %s)", id, err, backoff)
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		log.Printf("conn %d: connected", id)
		c.runConn(id, conn)
		log.Printf("conn %d: reconnecting in %s", id, backoff)
		time.Sleep(backoff)
	}
}

// runConn serves request frames on one connection until its read loop exits.
func (c *tunnelClient) runConn(id int, conn *websocket.Conn) {
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			log.Printf("conn %d: close: %v", id, errClose)
		}
	}()

	var writeMu sync.Mutex
	done := make(chan struct{})
	defer close(done)
	if c.pingInterval > 0 {
		go pingLoop(id, conn, &writeMu, done, c.pingInterval)
	}
	inflight := newInflightRequests()

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			log.Printf("conn %d: read: %v", id, err)
			return
		}
		var control controlFrame
		if err := json.Unmarshal(payload, &control); err == nil && control.Type == "cancel" {
			// The caller went away; abort the local request. Cancel frames
			// are only ever sent for streamed (proto 2) requests.
			inflight.cancel(control.ID)
			continue
		}
		var frame requestFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			log.Printf("conn %d: bad request frame: %v", id, err)
			continue
		}
		go c.handleRequest(id, conn, &writeMu, inflight, frame)
	}
}

// pingLoop sends keepalive pings until done is closed. A failed ping closes
// the connection so the reconnect loop replaces it.
func pingLoop(id int, conn *websocket.Conn, writeMu *sync.Mutex, done <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := conn.WriteMessage(websocket.PingMessage, nil)
			_ = conn.SetWriteDeadline(time.Time{})
			writeMu.Unlock()
			if err != nil {
				log.Printf("conn %d: ping: %v", id, err)
				if errClose := conn.Close(); errClose != nil {
					log.Printf("conn %d: close after ping failure: %v", id, errClose)
				}
				return
			}
		}
	}
}

// handleRequest forwards one request frame to the local proxy and returns
// its response: streamed as head/chunk/end frames when the request frame
// carries proto >= streamProtoVersion, otherwise as one buffered frame.
func (c *tunnelClient) handleRequest(id int, conn *websocket.Conn, writeMu *sync.Mutex, inflight *inflightRequests, frame requestFrame) {
	resp := responseFrame{ID: frame.ID}

	path := frame.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	method := strings.ToUpper(strings.TrimSpace(frame.Method))
	if method == "" {
		method = http.MethodGet
	}

	// A Worker cancel frame (caller gone, stream stalled) aborts the local
	// request through this context.
	ctx, cancel := context.WithCancel(context.Background())
	inflight.add(frame.ID, cancel)
	defer inflight.remove(frame.ID)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, c.target+path, bytes.NewReader(frame.Body))
	if err != nil {
		resp.Error = fmt.Sprintf("build request: %v", err)
		c.writeResponse(id, conn, writeMu, resp)
		return
	}
	for name, values := range frame.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		resp.Error = fmt.Sprintf("local request: %v", err)
		c.writeResponse(id, conn, writeMu, resp)
		return
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Printf("conn %d: close local response body: %v", id, errClose)
		}
	}()

	if frame.Proto >= streamProtoVersion {
		c.streamResponse(id, conn, writeMu, ctx, frame.ID, httpResp)
		return
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		resp.Error = fmt.Sprintf("read local response: %v", err)
		c.writeResponse(id, conn, writeMu, resp)
		return
	}
	resp.Status = httpResp.StatusCode
	resp.Headers = httpResp.Header.Clone()
	resp.Headers.Del("Content-Length")
	resp.Body = body
	c.writeResponse(id, conn, writeMu, resp)
}

// streamResponse sends a local response the protocol-v2 way: a head frame
// the moment the local service has produced its status and headers, then
// chunk frames as the body arrives, then an end frame. After the first
// chunk (sent immediately, preserving time-to-first-byte), body bytes are
// coalesced: a chunk frame goes out when the pending buffer reaches
// streamChunkSize, when streamCoalesceWindow elapses, or at end of body.
// A cancelled context (the Worker said the caller is gone) stops the
// stream silently — the Worker has already dropped the request, so there
// is nobody to report to.
func (c *tunnelClient) streamResponse(id int, conn *websocket.Conn, writeMu *sync.Mutex, ctx context.Context, frameID string, httpResp *http.Response) {
	head := responseFrame{
		ID:      frameID,
		Type:    "head",
		Status:  httpResp.StatusCode,
		Headers: httpResp.Header.Clone(),
	}
	head.Headers.Del("Content-Length")
	if !c.writeResponse(id, conn, writeMu, head) {
		return
	}

	// Reads block, so they run on their own goroutine and the loop below
	// can watch the coalesce timer and the cancel context between them.
	type readResult struct {
		data []byte
		err  error
	}
	reads := make(chan readResult, 1)
	go func() {
		buf := make([]byte, streamChunkSize)
		for {
			n, err := httpResp.Body.Read(buf)
			var data []byte
			if n > 0 {
				data = append([]byte(nil), buf[:n]...)
			}
			select {
			case reads <- readResult{data: data, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	flush := func(pending []byte) bool {
		if len(pending) == 0 {
			return true
		}
		return c.writeResponse(id, conn, writeMu, responseFrame{ID: frameID, Type: "chunk", Body: pending})
	}

	var pending []byte
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer, timerC = nil, nil
		}
	}
	defer stopTimer()
	sentFirst := false

	for {
		select {
		case r := <-reads:
			if len(r.data) > 0 {
				if !sentFirst {
					sentFirst = true
					if !flush(r.data) {
						return
					}
				} else {
					pending = append(pending, r.data...)
					switch {
					case len(pending) >= streamChunkSize:
						if !flush(pending) {
							return
						}
						pending = nil
						stopTimer()
					case timer == nil:
						timer = time.NewTimer(streamCoalesceWindow)
						timerC = timer.C
					}
				}
			}
			switch {
			case r.err == io.EOF:
				if !flush(pending) {
					return
				}
				c.writeResponse(id, conn, writeMu, responseFrame{ID: frameID, Type: "end"})
				return
			case r.err != nil:
				if ctx.Err() != nil {
					return // cancelled by the Worker; stay silent
				}
				if !flush(pending) {
					return
				}
				c.writeResponse(id, conn, writeMu, responseFrame{
					ID: frameID, Type: "error",
					Error: fmt.Sprintf("read local response: %v", r.err),
				})
				return
			}
		case <-timerC:
			timer, timerC = nil, nil
			if !flush(pending) {
				return
			}
			pending = nil
		case <-ctx.Done():
			return // cancelled by the Worker; stay silent
		}
	}
}

// writeResponse marshals and sends one frame, reporting whether the write
// succeeded. Frame writes on a connection are serialized through writeMu.
func (c *tunnelClient) writeResponse(id int, conn *websocket.Conn, writeMu *sync.Mutex, resp responseFrame) bool {
	payload, err := json.Marshal(resp)
	if err != nil {
		log.Printf("conn %d: marshal response %s: %v", id, resp.ID, err)
		return false
	}
	writeMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, payload)
	writeMu.Unlock()
	if err != nil {
		log.Printf("conn %d: write response %s: %v", id, resp.ID, err)
		return false
	}
	return true
}
