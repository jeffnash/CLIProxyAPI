package helps

import (
	"errors"
	"net/http"
	"testing"
)

func TestParseComposerWorkspaceOptionalAndValidated(t *testing.T) {
	env, err := ParseComposerWorkspace(http.Header{})
	if err != nil || env.Cwd != "" || env.Workspace != "" {
		t.Fatalf("headerless harness must receive neutral identity, got %#v %v", env, err)
	}
	env, err = ParseComposerWorkspace(http.Header{"X-Cwd": []string{"/repo"}})
	if err != nil || env.Cwd != "/repo" || env.Workspace != "/repo" {
		t.Fatalf("one path should supply both identities, got %#v %v", env, err)
	}
	for _, invalid := range []string{"relative/path", "C:", "https://example.test/work"} {
		h := http.Header{"X-Cwd": []string{invalid}, "X-Workspace-Path": []string{"/workspace/real"}}
		if _, err := ParseComposerWorkspace(h); err == nil {
			t.Fatalf("expected %q to be rejected", invalid)
		}
	}
	_, err = ParseComposerWorkspace(http.Header{"X-Cwd": []string{"/a", "/b"}})
	var workspaceErr *ComposerWorkspaceError
	if !errors.As(err, &workspaceErr) || workspaceErr.StatusCode() != 422 {
		t.Fatalf("duplicate path must remain a typed 422, got %T %v", err, err)
	}
}
