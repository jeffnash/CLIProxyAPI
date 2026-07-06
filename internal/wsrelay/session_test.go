package wsrelay

import "testing"

func TestDispatchToClosedPendingRequestDoesNotPanic(t *testing.T) {
	s := &session{closed: make(chan struct{})}
	req := newPendingRequest()
	s.pending.Store("req-1", req)
	req.close()

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("dispatch panicked after pending request closed: %v", recovered)
		}
	}()

	s.dispatch(Message{ID: "req-1", Type: MessageTypeHTTPResp})
}
