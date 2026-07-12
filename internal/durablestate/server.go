package durablestate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxUnixRequestLineBytes = 1 << 20 // 1 MiB

var errUnixRequestTooLarge = errors.New("unix request line exceeds size limit")

// Server serves the durable-state protocol over a Unix domain socket.
type Server struct {
	coord    *Coordinator
	listener net.Listener
	path     string

	mu      sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	baseCtx context.Context
}

// ListenUnix creates a Unix socket server at socketPath.
func ListenUnix(coord *Coordinator, socketPath string) (*Server, error) {
	if coord == nil {
		return nil, errors.New("coordinator required")
	}
	if socketPath == "" {
		return nil, errors.New("socket path required")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, err
	}
	if err := prepareUnixSocketPath(socketPath); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(socketPath)
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		coord:    coord,
		listener: ln,
		path:     socketPath,
		cancel:   cancel,
		baseCtx:  ctx,
	}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

// Path returns the Unix socket path.
func (s *Server) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close stops accepting connections and removes the socket file.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	err := s.listener.Close()
	s.wg.Wait()
	_ = os.Remove(s.path)
	return err
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.handleConn(c)
		}(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	for {
		select {
		case <-s.baseCtx.Done():
			return
		default:
		}
		line, err := readUnixRequestLine(reader)
		if errors.Is(err, errUnixRequestTooLarge) {
			resp := Response{Version: ProtocolVersion, OK: false, Code: CodeInvalidRequest, Error: err.Error()}
			_ = writeResponse(writer, resp)
			return
		}
		if err != nil {
			return
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			resp := Response{Version: ProtocolVersion, OK: false, Code: CodeInvalidRequest, Error: err.Error()}
			_ = writeResponse(writer, resp)
			continue
		}
		resp := s.coord.HandleRequest(s.baseCtx, req)
		if err := writeResponse(writer, resp); err != nil {
			return
		}
	}
}

func writeResponse(w *bufio.Writer, resp Response) error {
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

// Client is a thin Unix-socket client for the durable-state protocol.
type Client struct {
	path string
}

// NewClient dials requests against socketPath.
func NewClient(socketPath string) *Client {
	return &Client{path: socketPath}
}

// Call sends one request and waits for the matching response line.
func (c *Client) Call(ctx context.Context, req Request) (Response, error) {
	if req.Version == 0 {
		req.Version = ProtocolVersion
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", c.path)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = conn.Close() }()

	raw, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return Response{}, err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("%s: %s", resp.Code, resp.Error)
	}
	return resp, nil
}

func prepareUnixSocketPath(socketPath string) error {
	if _, err := os.Stat(socketPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("unix socket %s is already in use by a live coordinator", socketPath)
	}
	// Dial failed: treat as stale socket left by a dead process.
	if remErr := os.Remove(socketPath); remErr != nil && !os.IsNotExist(remErr) {
		return remErr
	}
	return nil
}

func readUnixRequestLine(br *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, min(maxUnixRequestLineBytes, 64*1024))
	for {
		fragment, err := br.ReadSlice('\n')
		if len(line)+len(fragment) > maxUnixRequestLineBytes {
			return nil, errUnixRequestTooLarge
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) == 0:
			return nil, io.EOF
		default:
			return nil, err
		}
	}
}
