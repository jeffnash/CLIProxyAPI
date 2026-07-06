package usage

import "testing"

func TestManagerQueueHonorsBufferLimit(t *testing.T) {
	m := NewManager(2)
	m.mu.Lock()
	m.enqueueLocked(queueItem{record: Record{Model: "first"}})
	m.enqueueLocked(queueItem{record: Record{Model: "second"}})
	m.enqueueLocked(queueItem{record: Record{Model: "third"}})
	if got := len(m.queue); got != 2 {
		t.Fatalf("queue len = %d, want 2", got)
	}
	if got := m.queue[0].record.Model; got != "second" {
		t.Fatalf("queue[0] = %q, want second", got)
	}
	if got := m.queue[1].record.Model; got != "third" {
		t.Fatalf("queue[1] = %q, want third", got)
	}
	m.mu.Unlock()
}
