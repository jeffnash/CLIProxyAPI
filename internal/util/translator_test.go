package util

import "testing"

// UT1 regression: MapToolName must not collapse two advertised tools that
// differ only by case or a leading underscore. "Foo" and "_foo" share the same
// canonical key ("foo"), so a canonical lookup alone would route both onto the
// single canonical original. The exact-match short-circuit returns any name
// that is itself an advertised (map value) original, unchanged.
func TestMapToolName_ExactMatchShortCircuit(t *testing.T) {
	// A map whose VALUES include both originals (the short-circuit scans the
	// original values, not the keys). Keys are arbitrary here; the point is that
	// both "Foo" and "_foo" are advertised tool names.
	m := map[string]string{
		"foo":     "Foo",
		"foo_alt": "_foo",
	}
	if got := MapToolName(m, "Foo"); got != "Foo" {
		t.Fatalf("exact 'Foo' must pass through unchanged, got %q", got)
	}
	if got := MapToolName(m, "_foo"); got != "_foo" {
		t.Fatalf("exact '_foo' must not collapse to 'Foo', got %q", got)
	}
}

// UT1 regression: a name that is NOT an advertised original still resolves
// through the canonical lowercase/underscore lookup (back-compat).
func TestMapToolName_CanonicalLookupStillWorks(t *testing.T) {
	m := map[string]string{"foo": "Foo"}
	// "FOO" is not an advertised value, so it falls through to the canonical
	// lookup (canonical "foo") and resolves to the original "Foo".
	if got := MapToolName(m, "FOO"); got != "Foo" {
		t.Fatalf("canonical lookup should map FOO -> Foo, got %q", got)
	}
	// Leading-underscore form likewise canonicalizes to "foo".
	if got := MapToolName(m, "_foo"); got != "Foo" {
		t.Fatalf("canonical lookup should map _foo -> Foo, got %q", got)
	}
}

// UT1 edge: nil map / empty name return the input unchanged.
func TestMapToolName_NilAndEmpty(t *testing.T) {
	if got := MapToolName(nil, "Foo"); got != "Foo" {
		t.Fatalf("nil map should return name unchanged, got %q", got)
	}
	if got := MapToolName(map[string]string{"foo": "Foo"}, ""); got != "" {
		t.Fatalf("empty name should return empty, got %q", got)
	}
}
