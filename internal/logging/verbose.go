package logging

import (
	"os"
	"strings"
	"sync/atomic"
)

var verboseEnabled atomic.Bool

func init() {
	if env := strings.ToLower(strings.TrimSpace(os.Getenv("VERBOSE_LOGGING"))); env != "" {
		switch env {
		case "1", "true", "yes", "y", "on":
			verboseEnabled.Store(true)
		case "0", "false", "no", "n", "off":
			verboseEnabled.Store(false)
		}
	}
}

// VerboseEnabled returns whether verbose logging is enabled.
// This is used to gate request/response snippet capture in hot paths.
func VerboseEnabled() bool {
	return verboseEnabled.Load()
}

// SetVerboseEnabled updates the verbose logging toggle at runtime.
// Note: this does not adjust log levels; it only gates snippet capture.
func SetVerboseEnabled(enabled bool) {
	verboseEnabled.Store(enabled)
}
