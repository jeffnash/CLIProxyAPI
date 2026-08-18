package helps

import "fmt"

type exactEventSegment struct {
	kind string
	data string
}

// ExactEventPrefixMatcher retains a bounded ordered text/reasoning prefix from
// an interrupted stream and removes that exact prefix from one recovery
// stream. Adjacent events of the same kind are coalesced, so transport chunk
// boundaries do not affect comparison; event kind and bytes remain strict.
type ExactEventPrefixMatcher struct {
	maxBytes   int
	bytes      int
	segments   []exactEventSegment
	recovering bool
	segment    int
	offset     int
}

func NewExactEventPrefixMatcher(maxBytes int) (*ExactEventPrefixMatcher, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("exact event prefix capacity must be positive")
	}
	return &ExactEventPrefixMatcher{maxBytes: maxBytes}, nil
}

func (m *ExactEventPrefixMatcher) Record(kind, delta string) error {
	if m == nil || m.recovering {
		return fmt.Errorf("exact event prefix is not recordable")
	}
	if kind != "text" && kind != "reasoning" {
		return fmt.Errorf("unsupported exact event kind %q", kind)
	}
	if len(delta) > m.maxBytes-m.bytes {
		return fmt.Errorf("exact event prefix exceeds %d bytes", m.maxBytes)
	}
	if delta == "" {
		return nil
	}
	m.bytes += len(delta)
	last := len(m.segments) - 1
	if last >= 0 && m.segments[last].kind == kind {
		m.segments[last].data += delta
		return nil
	}
	m.segments = append(m.segments, exactEventSegment{kind: kind, data: delta})
	return nil
}

func (m *ExactEventPrefixMatcher) StartRecovery() {
	if m == nil {
		return
	}
	m.recovering = true
	m.segment = 0
	m.offset = 0
}

func (m *ExactEventPrefixMatcher) Complete() bool {
	return m != nil && m.recovering && m.segment >= len(m.segments)
}

func (m *ExactEventPrefixMatcher) Consume(kind, delta string) (string, error) {
	if m == nil || !m.recovering {
		return "", fmt.Errorf("exact event prefix recovery has not started")
	}
	if m.Complete() {
		return delta, nil
	}
	remaining := delta
	for remaining != "" && !m.Complete() {
		segment := m.segments[m.segment]
		if kind != segment.kind {
			return "", fmt.Errorf("recovery event kind %q diverged from %q", kind, segment.kind)
		}
		want := segment.data[m.offset:]
		compare := len(remaining)
		if compare > len(want) {
			compare = len(want)
		}
		if remaining[:compare] != want[:compare] {
			return "", fmt.Errorf("recovery event bytes diverged from the emitted prefix")
		}
		remaining = remaining[compare:]
		m.offset += compare
		if m.offset == len(segment.data) {
			m.segment++
			m.offset = 0
			if remaining != "" && !m.Complete() && m.segments[m.segment].kind != kind {
				return "", fmt.Errorf("recovery event crossed an ordered kind boundary")
			}
		}
	}
	if m.Complete() {
		return remaining, nil
	}
	return "", nil
}
