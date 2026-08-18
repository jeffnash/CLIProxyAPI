package helps

import "testing"

func TestExactEventPrefixMatcherIgnoresChunkBoundariesAndReturnsOnlySuffix(t *testing.T) {
	m, err := NewExactEventPrefixMatcher(64)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct{ kind, delta string }{
		{"reasoning", "think"},
		{"reasoning", "ing"},
		{"text", "hello "},
		{"text", "world"},
	} {
		if err := m.Record(event.kind, event.delta); err != nil {
			t.Fatal(err)
		}
	}
	m.StartRecovery()

	for _, event := range []struct{ kind, delta string }{
		{"reasoning", "thi"},
		{"reasoning", "nking"},
		{"text", "hello"},
	} {
		suffix, err := m.Consume(event.kind, event.delta)
		if err != nil {
			t.Fatal(err)
		}
		if suffix != "" {
			t.Fatalf("matched prefix leaked %q", suffix)
		}
	}
	suffix, err := m.Consume("text", " world and beyond")
	if err != nil {
		t.Fatal(err)
	}
	if suffix != " and beyond" {
		t.Fatalf("suffix = %q, want %q", suffix, " and beyond")
	}
	if !m.Complete() {
		t.Fatal("matcher did not complete the exact prefix")
	}
}

func TestExactEventPrefixMatcherFailsClosedOnKindByteAndCapacityDivergence(t *testing.T) {
	m, err := NewExactEventPrefixMatcher(5)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Record("text", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := m.Record("text", "!"); err == nil {
		t.Fatal("capacity overflow was accepted")
	}

	for _, tc := range []struct {
		name string
		kind string
		text string
	}{
		{"kind", "reasoning", "hello"},
		{"bytes", "text", "hullo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matcher, err := NewExactEventPrefixMatcher(16)
			if err != nil {
				t.Fatal(err)
			}
			if err := matcher.Record("text", "hello"); err != nil {
				t.Fatal(err)
			}
			matcher.StartRecovery()
			if _, err := matcher.Consume(tc.kind, tc.text); err == nil {
				t.Fatal("divergent recovery prefix was accepted")
			}
		})
	}
}
