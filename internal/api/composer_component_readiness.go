package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/durablestate"
)

const (
	stateEncryptionRequiredEnv = "CLIPROXY_STATE_ENCRYPTION_REQUIRED"
	stateSocketEnv             = "CLIPROXY_STATE_SOCKET"
	stateRequireWriterLeaseEnv = "CLIPROXY_STATE_REQUIRE_WRITER_LEASE"
)

func stateEncryptionRequired() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(stateEncryptionRequiredEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func stateRequireWriterLease() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(stateRequireWriterLeaseEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// CursorComposerReadiness is the structured readiness snapshot for /ready.
type CursorComposerReadiness struct {
	Lease      *durablestate.LeaseRecord       `json:"lease,omitempty"`
	Metrics    map[string]int64                `json:"metrics,omitempty"`
	Flags      durablestate.FeatureFlags       `json:"flags,omitempty"`
	Migration  *durablestate.MigrationSnapshot `json:"migration,omitempty"`
	Draining   bool                            `json:"draining,omitempty"`
	StateEpoch int64                           `json:"state_epoch,omitempty"`
}

// checkCursorComposerComponentReadiness probes bridge, encryption key, state socket, and writer lease.
func checkCursorComposerComponentReadiness(ctx context.Context) error {
	_, err := probeCursorComposerComponent(ctx)
	return err
}

// probeCursorComposerComponent returns readiness details when the component is ready.
func probeCursorComposerComponent(ctx context.Context) (*CursorComposerReadiness, error) {
	if err := checkComposerBridgeReadiness(ctx); err != nil {
		return nil, err
	}
	if stateEncryptionRequired() {
		provider, err := durablestate.LoadRotatingMasterKeyProvider(true)
		if err != nil {
			return nil, fmt.Errorf("cursor-composer encryption: %w", err)
		}
		if provider == nil || provider.CurrentKeyID() == "" {
			return nil, fmt.Errorf("cursor-composer encryption key unavailable")
		}
	}
	out := &CursorComposerReadiness{
		Metrics: durablestate.DefaultMetrics.Snapshot(),
		Flags:   durablestate.LoadFeatureFlagsFromEnv(),
	}
	socket := strings.TrimSpace(os.Getenv(stateSocketEnv))
	if socket == "" {
		return out, nil
	}
	client := durablestate.NewClient(socket)
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := client.Call(pingCtx, durablestate.Request{Op: durablestate.OpPing}); err != nil {
		return nil, fmt.Errorf("cursor-composer state backend: %w", err)
	}

	leaseCtx, leaseCancel := context.WithTimeout(ctx, 2*time.Second)
	defer leaseCancel()
	leaseResp, leaseErr := client.Call(leaseCtx, durablestate.Request{Op: durablestate.OpCurrentLease})
	if leaseErr != nil {
		if stateRequireWriterLease() || !isLeaseNotFound(leaseErr, leaseResp) {
			if stateRequireWriterLease() && isLeaseNotFound(leaseErr, leaseResp) {
				return nil, fmt.Errorf("cursor-composer writer lease required but missing")
			}
			if !isLeaseNotFound(leaseErr, leaseResp) {
				return nil, fmt.Errorf("cursor-composer writer lease: %w", leaseErr)
			}
		}
	} else {
		var lease durablestate.LeaseRecord
		if err := json.Unmarshal(leaseResp.Payload, &lease); err != nil {
			return nil, fmt.Errorf("cursor-composer writer lease decode: %w", err)
		}
		if !lease.ExpiresAt.IsZero() && lease.ExpiresAt.Before(time.Now().UTC()) {
			return nil, fmt.Errorf("cursor-composer writer lease expired")
		}
		out.Lease = &lease
		out.StateEpoch = lease.StateEpoch
	}

	metricsCtx, metricsCancel := context.WithTimeout(ctx, 2*time.Second)
	defer metricsCancel()
	if metricsResp, err := client.Call(metricsCtx, durablestate.Request{Op: durablestate.OpMetrics}); err == nil {
		var payload struct {
			Metrics map[string]int64          `json:"metrics"`
			Flags   durablestate.FeatureFlags `json:"flags"`
		}
		if err := json.Unmarshal(metricsResp.Payload, &payload); err == nil {
			if len(payload.Metrics) > 0 {
				out.Metrics = payload.Metrics
			}
			out.Flags = payload.Flags
		}
	}

	snapCtx, snapCancel := context.WithTimeout(ctx, 2*time.Second)
	defer snapCancel()
	if snapResp, err := client.Call(snapCtx, durablestate.Request{Op: durablestate.OpSnapshotState}); err == nil {
		var snap durablestate.MigrationSnapshot
		if err := json.Unmarshal(snapResp.Payload, &snap); err == nil {
			out.Migration = &snap
			out.StateEpoch = snap.StateEpoch
			if n := snap.InvocationCounts["maybe_accepted"] + snap.InvocationCounts["accepted"]; n > 0 {
				if out.Metrics == nil {
					out.Metrics = map[string]int64{}
				}
				out.Metrics["unresolved_post_send"] = n
			}
		}
	} else if !strings.Contains(err.Error(), "unknown op") {
		// Snapshot failures indicate migration/backend unreadiness when the socket is up.
		return nil, fmt.Errorf("cursor-composer migration snapshot: %w", err)
	}

	return out, nil
}

func isLeaseNotFound(err error, resp durablestate.Response) bool {
	if resp.Code == durablestate.CodeNotFound {
		return true
	}
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not_found") || strings.Contains(msg, "no writer lease")
}
