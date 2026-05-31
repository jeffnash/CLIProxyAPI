package executor

import "regexp"

// composerControlTokenPattern matches </think>, <|final|>, full-width variants,
// and any whitespace-padded forms. Used by cursorTextSanitizer in cursor_executor.go.
var composerControlTokenPattern = regexp.MustCompile(`</think>|<\s*[|\x{FF5C}]\s*final\s*[|\x{FF5C}]\s*>`)
