package util

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackedReadCloser struct {
	reader *strings.Reader
	closed *atomic.Int32
}

func (b *trackedReadCloser) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *trackedReadCloser) Close() error {
	b.closed.Add(1)
	return nil
}

func TestGetPublicIPClosesFailedAttemptBodies(t *testing.T) {
	oldServices := ipServices
	oldClient := http.DefaultClient
	defer func() {
		ipServices = oldServices
		http.DefaultClient = oldClient
	}()

	ipServices = []string{"https://example.invalid/first", "https://example.invalid/second"}
	var firstClosed atomic.Int32
	var secondClosed atomic.Int32
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/first":
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       &trackedReadCloser{reader: strings.NewReader("error"), closed: &firstClosed},
				Header:     make(http.Header),
				Request:    req,
			}, nil
		case "/second":
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &trackedReadCloser{reader: strings.NewReader("203.0.113.7\n"), closed: &secondClosed},
				Header:     make(http.Header),
				Request:    req,
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
	})}

	ip, err := getPublicIP()
	if err != nil {
		t.Fatalf("getPublicIP() error = %v", err)
	}
	if ip != "203.0.113.7" {
		t.Fatalf("getPublicIP() = %q, want test IP", ip)
	}
	if got := firstClosed.Load(); got != 1 {
		t.Fatalf("first response close count = %d, want 1", got)
	}
	if got := secondClosed.Load(); got != 1 {
		t.Fatalf("second response close count = %d, want 1", got)
	}
}
