package helps

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

const (
	MaxWorkspacePathBytes = 4096
	MaxOptionalFieldBytes = 256
)

// ComposerClientEnv is typed environment from X-Cwd etc headers
type ComposerClientEnv struct {
	Cwd       string
	Workspace string
	Shell     string
	OsVersion string
}

// ComposerWorkspaceError preserves structured parser diagnostics for logging and tests. Cursor Composer
// deliberately treats these header-validation failures as advisory-only and falls back to neutral context;
// its Status value is not surfaced as an API response.
type ComposerWorkspaceError struct {
	Code    string // workspace_required, workspace_invalid, workspace_changed
	Message string
	Status  int
}

func (e *ComposerWorkspaceError) Error() string   { return e.Message }
func (e *ComposerWorkspaceError) StatusCode() int { return e.Status }

// ParseComposerWorkspace parses X-Cwd and X-Workspace-Path etc from headers
// Workspace headers are optional for compatibility with unwrapped harnesses. When one path is present it
// supplies both identities; when neither is present the bridge uses its isolated neutral context.
func ParseComposerWorkspace(h http.Header) (*ComposerClientEnv, error) {
	cwds := h.Values("X-Cwd")
	workspaces := h.Values("X-Workspace-Path")
	if len(cwds) == 0 && len(workspaces) == 0 {
		return &ComposerClientEnv{}, nil
	}
	if len(cwds) > 1 {
		return nil, &ComposerWorkspaceError{Code: "workspace_invalid", Message: "duplicate X-Cwd", Status: 422}
	}
	if len(workspaces) > 1 {
		return nil, &ComposerWorkspaceError{Code: "workspace_invalid", Message: "duplicate X-Workspace-Path", Status: 422}
	}
	var cwd, ws string
	if len(cwds) == 1 {
		cwd = cwds[0]
	}
	if len(workspaces) == 1 {
		ws = workspaces[0]
	}
	if cwd == "" {
		cwd = ws
	}
	if ws == "" {
		ws = cwd
	}
	if err := validateWorkspaceField(cwd, true); err != nil {
		return nil, err
	}
	if err := validateWorkspaceField(ws, true); err != nil {
		return nil, err
	}
	shells := h.Values("X-Shell")
	osVers := h.Values("X-Os-Version")
	var shell, osVer string
	if len(shells) > 1 {
		return nil, &ComposerWorkspaceError{Code: "workspace_invalid", Message: "duplicate X-Shell", Status: 422}
	}
	if len(shells) == 1 {
		shell = shells[0]
		if err := validateWorkspaceField(shell, false); err != nil {
			return nil, err
		}
	}
	if len(osVers) > 1 {
		return nil, &ComposerWorkspaceError{Code: "workspace_invalid", Message: "duplicate X-Os-Version", Status: 422}
	}
	if len(osVers) == 1 {
		osVer = osVers[0]
		if err := validateWorkspaceField(osVer, false); err != nil {
			return nil, err
		}
	}

	env := &ComposerClientEnv{
		Cwd:       cwd,
		Workspace: ws,
		Shell:     shell,
		OsVersion: osVer,
	}
	return env, nil
}

func validateWorkspaceField(v string, required bool) error {
	if required && strings.TrimSpace(v) == "" {
		return &ComposerWorkspaceError{Code: "workspace_required", Message: "empty workspace field", Status: 422}
	}
	if len(v) > MaxWorkspacePathBytes && required {
		return &ComposerWorkspaceError{Code: "workspace_invalid", Message: fmt.Sprintf("field too large %d > %d", len(v), MaxWorkspacePathBytes), Status: 422}
	}
	if len(v) > MaxOptionalFieldBytes && !required {
		return &ComposerWorkspaceError{Code: "workspace_invalid", Message: fmt.Sprintf("optional field too large %d > %d", len(v), MaxOptionalFieldBytes), Status: 422}
	}
	for _, r := range v {
		if r == 0 || r == '\r' || r == '\n' || r == 127 {
			return &ComposerWorkspaceError{Code: "workspace_invalid", Message: "field contains NUL/CR/LF/DEL", Status: 422}
		}
		if r < 32 {
			return &ComposerWorkspaceError{Code: "workspace_invalid", Message: "field contains control character", Status: 422}
		}
		if !unicode.IsPrint(r) {
			return &ComposerWorkspaceError{Code: "workspace_invalid", Message: "field contains non-printable character", Status: 422}
		}
	}
	if required {
		if err := validatePathLexical(v); err != nil {
			return err
		}
	}
	return nil
}

func validatePathLexical(p string) error {
	if p == "" {
		return &ComposerWorkspaceError{Code: "workspace_invalid", Message: "empty path", Status: 422}
	}
	// Reject URLs
	if strings.Contains(p, "://") {
		return &ComposerWorkspaceError{Code: "workspace_invalid", Message: "path is URL", Status: 422}
	}
	// Allow explicitly supplied /workspace (per checklist: only synthesized /workspace is forbidden, but explicit is allowed)
	// So we allow /workspace as valid.
	// Check POSIX absolute: starts with /
	if strings.HasPrefix(p, "/") {
		return nil
	}
	// Windows drive-absolute: e.g. C:\\ or C:/. A bare C: is drive-relative.
	if len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return nil
	}
	// UNC: \\server\share or //server/share
	if strings.HasPrefix(p, "\\\\") || strings.HasPrefix(p, "//") {
		return nil
	}
	return &ComposerWorkspaceError{Code: "workspace_invalid", Message: "path not absolute: " + p, Status: 422}
}
