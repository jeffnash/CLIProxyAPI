package handlers

import (
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

func TestPendingStreamErrorReturnsQueuedError(t *testing.T) {
	errs := make(chan *interfaces.ErrorMessage, 1)
	want := &interfaces.ErrorMessage{Error: errors.New("upstream failed")}
	errs <- want

	got, ok := PendingStreamError(errs)
	if !ok {
		t.Fatal("PendingStreamError ok = false, want true")
	}
	if got != want {
		t.Fatalf("PendingStreamError = %#v, want %#v", got, want)
	}
}

func TestPendingStreamErrorDoesNotBlock(t *testing.T) {
	errs := make(chan *interfaces.ErrorMessage)
	if got, ok := PendingStreamError(errs); ok || got != nil {
		t.Fatalf("PendingStreamError = %#v, %t; want nil, false", got, ok)
	}
}
