// Command wstunnel-client exposes a local CLIProxyAPI instance through a
// Cloudflare Worker + Durable Object. The Durable Object accepts client HTTP
// requests and forwards each one as a JSON frame over a persistent WebSocket
// connection held by this client; the client calls the local proxy and
// returns the whole response as one frame.
//
// Frame protocol (one JSON object per text message):
//
//	request:  {"id":"...","method":"POST","path":"/v1/...","headers":{...},"body":"<base64>"}
//	response: {"id":"...","status":200,"headers":{...},"body":"<base64>"}
//	          {"id":"...","error":"..."}
//
// Long generations leave the socket silent for minutes while the local
// response streams in, so each connection sends WebSocket pings on an
// interval (see -ping-interval) to keep intermediaries from reaping it as
// idle. Cloudflare answers protocol pings at the platform level.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type requestFrame struct {
	ID      string      `json:"id"`
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

type responseFrame struct {
	ID      string      `json:"id"`
	Status  int         `json:"status,omitempty"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
	Error   string      `json:"error,omitempty"`
}

type tunnelClient struct {
	workerURL    string
	target       string
	pingInterval time.Duration
	httpClient   *http.Client
}

func main() {
	worker := flag.String("worker", "", "Worker WebSocket URL (wss://...)")
	target := flag.String("target", "http://127.0.0.1:8317", "local proxy base URL")
	conns := flag.Int("conns", 4, "number of persistent tunnel connections")
	pingInterval := flag.Duration("ping-interval", 20*time.Second, "WebSocket keepalive ping interval (0 disables)")
	flag.Parse()

	if strings.TrimSpace(*worker) == "" {
		fmt.Fprintln(os.Stderr, "wstunnel-client: -worker is required")
		os.Exit(2)
	}
	if *conns < 1 {
		fmt.Fprintln(os.Stderr, "wstunnel-client: -conns must be >= 1")
		os.Exit(2)
	}

	client := &tunnelClient{
		workerURL:    *worker,
		target:       strings.TrimSuffix(*target, "/"),
		pingInterval: *pingInterval,
		// No timeout: proxied generations legitimately run for minutes.
		httpClient: &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	log.Printf("wstunnel-client: %d connection(s) to %s, target %s, ping %s", *conns, *worker, client.target, *pingInterval)
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
		conn, _, err := websocket.DefaultDialer.Dial(c.workerURL, nil)
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

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			log.Printf("conn %d: read: %v", id, err)
			return
		}
		var frame requestFrame
		if err := json.Unmarshal(payload, &frame); err != nil {
			log.Printf("conn %d: bad request frame: %v", id, err)
			continue
		}
		go c.handleRequest(id, conn, &writeMu, frame)
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

// handleRequest forwards one request frame to the local proxy and writes the
// whole response back as one frame.
func (c *tunnelClient) handleRequest(id int, conn *websocket.Conn, writeMu *sync.Mutex, frame requestFrame) {
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

	req, err := http.NewRequest(method, c.target+path, bytes.NewReader(frame.Body))
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

func (c *tunnelClient) writeResponse(id int, conn *websocket.Conn, writeMu *sync.Mutex, resp responseFrame) {
	payload, err := json.Marshal(resp)
	if err != nil {
		log.Printf("conn %d: marshal response %s: %v", id, resp.ID, err)
		return
	}
	writeMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, payload)
	writeMu.Unlock()
	if err != nil {
		log.Printf("conn %d: write response %s: %v", id, resp.ID, err)
	}
}
