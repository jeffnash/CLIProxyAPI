package handlers

import "github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"

// PendingStreamError returns an already-queued stream startup error without blocking.
func PendingStreamError(errs <-chan *interfaces.ErrorMessage) (*interfaces.ErrorMessage, bool) {
	if errs == nil {
		return nil, false
	}
	select {
	case errMsg, ok := <-errs:
		if !ok {
			return nil, false
		}
		return errMsg, true
	default:
		return nil, false
	}
}
