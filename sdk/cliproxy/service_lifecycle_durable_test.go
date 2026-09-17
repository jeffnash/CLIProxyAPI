package cliproxy

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/durablestate"
)

// TestServiceStartDurableRuntime_AttachesCoordinator guards the Railway boot
// contract: when CLIPROXY_STATE_SOCKET is configured, the service must boot
// the Go-owned coordinator (Unix socket + writer lease). The start script
// blocks /readyz on that socket, so a missing startup wedges every deploy.
func TestServiceStartDurableRuntime_AttachesCoordinator(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "state.sock")
	t.Setenv("CLIPROXY_STATE_SOCKET", socket)
	t.Setenv("CLIPROXY_STATE_SQLITE_PATH", filepath.Join(dir, "durable-state.sqlite"))
	t.Setenv("CLIPROXY_STATE_POSTGRES_DSN", "")

	svc := &Service{}
	if err := svc.startDurableRuntime(context.Background()); err != nil {
		t.Fatalf("startDurableRuntime error: %v", err)
	}
	if svc.durableRuntime == nil {
		t.Fatal("expected durableRuntime to be attached")
	}
	t.Cleanup(func() {
		_ = svc.durableRuntime.Close(context.Background())
	})

	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		t.Fatalf("coordinator socket %s not listening: %v", socket, err)
	}
	_ = conn.Close()
}

// TestServiceStartDurableRuntime_SkipsWithoutSocket ensures the coordinator
// stays off when no socket is configured.
func TestServiceStartDurableRuntime_SkipsWithoutSocket(t *testing.T) {
	t.Setenv("CLIPROXY_STATE_SOCKET", "")
	t.Setenv("CLIPROXY_STATE_POSTGRES_DSN", "")

	svc := &Service{}
	if err := svc.startDurableRuntime(context.Background()); err != nil {
		t.Fatalf("startDurableRuntime error: %v", err)
	}
	if svc.durableRuntime != nil {
		t.Fatal("expected no durableRuntime without CLIPROXY_STATE_SOCKET")
	}
}

// TestServiceShutdown_ClosesDurableRuntime ensures Shutdown releases an
// attached coordinator (socket closed, lease released).
func TestServiceShutdown_ClosesDurableRuntime(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "state.sock")
	rt, err := durablestate.OpenRuntime(context.Background(), durablestate.RuntimeConfig{
		Socket:     socket,
		SQLitePath: filepath.Join(dir, "durable-state.sqlite"),
	})
	if err != nil {
		t.Fatalf("OpenRuntime error: %v", err)
	}

	svc := &Service{durableRuntime: rt}
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}
	if svc.durableRuntime != nil {
		t.Fatal("expected durableRuntime to be detached after Shutdown")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("expected socket %s to be removed, stat err: %v", socket, err)
	}
}

func TestRuntimeBackendKind(t *testing.T) {
	if got := runtimeBackendKind(durablestate.RuntimeConfig{PostgresDSN: "postgres://x"}); got != "postgres" {
		t.Fatalf("postgres DSN kind = %q, want postgres", got)
	}
	if got := runtimeBackendKind(durablestate.RuntimeConfig{}); got != "sqlite" {
		t.Fatalf("default kind = %q, want sqlite", got)
	}
}
