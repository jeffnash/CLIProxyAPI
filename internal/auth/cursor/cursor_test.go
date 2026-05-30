package cursor

import (
	"testing"
)

func TestResolveCursorClientVersionDefault(t *testing.T) {
	t.Setenv("CURSOR_CLIENT_VERSION", "")
	if got := ResolveCursorClientVersion(); got != CursorClientVersion {
		t.Errorf("default version mismatch: got %q want %q", got, CursorClientVersion)
	}
}

func TestResolveCursorClientVersionEnvOverrideBare(t *testing.T) {
	t.Setenv("CURSOR_CLIENT_VERSION", "2099.01.01-test")
	if got := ResolveCursorClientVersion(); got != "2099.01.01-test" {
		t.Errorf("bare override: got %q want %q", got, "2099.01.01-test")
	}
}

func TestResolveCursorClientVersionEnvOverrideWithPrefix(t *testing.T) {
	// "cli-" prefix is stripped so headers don't end up with "cli-cli-…".
	t.Setenv("CURSOR_CLIENT_VERSION", "cli-2099.02.02-prefixed")
	if got := ResolveCursorClientVersion(); got != "2099.02.02-prefixed" {
		t.Errorf("prefixed override: got %q want %q", got, "2099.02.02-prefixed")
	}
}

func TestBuildCursorIdentityHeadersAppliesOverride(t *testing.T) {
	t.Setenv("CURSOR_CLIENT_VERSION", "9999.12.31-abc")
	headers := BuildCursorIdentityHeaders()
	if headers["x-cursor-client-version"] != "9999.12.31-abc" {
		t.Errorf("identity header mismatch: got %q", headers["x-cursor-client-version"])
	}
	if headers["x-cursor-client-type"] != CursorClientType {
		t.Errorf("client-type header mismatch: got %q", headers["x-cursor-client-type"])
	}
}

func TestBuildCursorIdentityHeadersStripsLegacyCLIPrefix(t *testing.T) {
	// Legacy configs may still pass CURSOR_CLIENT_VERSION="cli-X.Y.Z".
	// We strip the cli- prefix so the header ends up clean (IDE-style).
	t.Setenv("CURSOR_CLIENT_VERSION", "cli-9999.12.31-xyz")
	headers := BuildCursorIdentityHeaders()
	if headers["x-cursor-client-version"] != "9999.12.31-xyz" {
		t.Errorf("identity header should normalise cli-prefix: got %q", headers["x-cursor-client-version"])
	}
}

func TestBuildCursorIdentityHeadersIDEByDefault(t *testing.T) {
	headers := BuildCursorIdentityHeaders()
	if headers["x-cursor-client-type"] != "ide" {
		t.Errorf("expected x-cursor-client-type=ide, got %q", headers["x-cursor-client-type"])
	}
	if headers["x-cursor-client-version"] != "3.5.38" {
		t.Errorf("expected x-cursor-client-version=3.5.38, got %q", headers["x-cursor-client-version"])
	}
}
