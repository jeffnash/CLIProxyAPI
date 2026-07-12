package durablestate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	stateSocketEnv         = "CLIPROXY_STATE_SOCKET"
	statePostgresDSNEnv    = "CLIPROXY_STATE_POSTGRES_DSN"
	stateSQLitePathEnv     = "CLIPROXY_STATE_SQLITE_PATH"
	stateBlobRootEnv       = "CLIPROXY_STATE_BLOB_ROOT"
	stateEpochEnv          = "CLIPROXY_STATE_EPOCH"
	stateInstanceIDEnv     = "CLIPROXY_STATE_INSTANCE_ID"
	stateLeaseTTLEnv       = "CLIPROXY_STATE_LEASE_TTL_MS"
	stateMaxReservedEnv    = "CLIPROXY_STATE_MAX_RESERVED_BYTES"
	stateEmergencyEnv      = "CLIPROXY_STATE_EMERGENCY_RESERVE_BYTES"
	stateTenantMaxEnv      = "CLIPROXY_STATE_TENANT_MAX_RESERVED_BYTES"
	stateBinaryVersionEnv  = "CLIPROXY_STATE_BINARY_VERSION"
	defaultStateLeaseTTLMS = 15000
)

// Runtime owns the durable-state coordinator process: backend, unix server, lease, blobs.
type Runtime struct {
	Backend  Backend
	Coord    *Coordinator
	Server   *Server
	Blobs    *BlobStore
	Keys     *RotatingMasterKeyProvider
	Lease    *LeaseRecord
	Socket   string
	Instance string
	Epoch    int64
	Backoff  RestartBackoff

	mu       sync.Mutex
	leaseTTL time.Duration
	stopHB   context.CancelFunc
	hbWG     sync.WaitGroup
}

// RuntimeConfig configures OpenRuntime.
type RuntimeConfig struct {
	Socket                 string
	PostgresDSN            string
	SQLitePath             string
	BlobRoot               string
	StateEpoch             int64
	InstanceID             string
	BinaryVersion          string
	LeaseTTL               time.Duration
	MaxReservedBytes       int64
	EmergencyReserveBytes  int64
	TenantMaxReservedBytes int64
	EncryptionRequired     bool
}

// LoadRuntimeConfigFromEnv reads CLIPROXY_STATE_* and related flags.
func LoadRuntimeConfigFromEnv() RuntimeConfig {
	ttlMS, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(stateLeaseTTLEnv)), 10, 64)
	if ttlMS <= 0 {
		ttlMS = defaultStateLeaseTTLMS
	}
	epoch, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(stateEpochEnv)), 10, 64)
	maxBytes, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(stateMaxReservedEnv)), 10, 64)
	emergency, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(stateEmergencyEnv)), 10, 64)
	tenantMax, _ := strconv.ParseInt(strings.TrimSpace(os.Getenv(stateTenantMaxEnv)), 10, 64)
	return RuntimeConfig{
		Socket:                 strings.TrimSpace(os.Getenv(stateSocketEnv)),
		PostgresDSN:            strings.TrimSpace(os.Getenv(statePostgresDSNEnv)),
		SQLitePath:             strings.TrimSpace(os.Getenv(stateSQLitePathEnv)),
		BlobRoot:               strings.TrimSpace(os.Getenv(stateBlobRootEnv)),
		StateEpoch:             epoch,
		InstanceID:             strings.TrimSpace(os.Getenv(stateInstanceIDEnv)),
		BinaryVersion:          strings.TrimSpace(os.Getenv(stateBinaryVersionEnv)),
		LeaseTTL:               time.Duration(ttlMS) * time.Millisecond,
		MaxReservedBytes:       maxBytes,
		EmergencyReserveBytes:  emergency,
		TenantMaxReservedBytes: tenantMax,
		EncryptionRequired:     envBool("CLIPROXY_STATE_ENCRYPTION_REQUIRED", false) || envBool("CLIPROXY_FLAG_ENCRYPTED_STATE", false),
	}
}

// ShouldStartRuntime reports whether the state coordinator should bind.
func ShouldStartRuntime(cfg RuntimeConfig) bool {
	flags := LoadFeatureFlagsFromEnv()
	if !flags.StateCoordinator {
		return false
	}
	return cfg.Socket != ""
}

// OpenRuntime opens backend + unix server + writer lease from cfg.
func OpenRuntime(ctx context.Context, cfg RuntimeConfig) (*Runtime, error) {
	if cfg.Socket == "" {
		return nil, fmt.Errorf("CLIPROXY_STATE_SOCKET required")
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = "inst-" + uuid.NewString()
	}
	if cfg.BinaryVersion == "" {
		cfg.BinaryVersion = "cliproxy-durable-state"
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = defaultStateLeaseTTLMS * time.Millisecond
	}
	if cfg.StateEpoch <= 0 {
		cfg.StateEpoch = 1
	}
	if cfg.SQLitePath == "" && cfg.PostgresDSN == "" {
		cfg.SQLitePath = filepath.Join(filepath.Dir(cfg.Socket), "durable-state.sqlite")
	}

	keys, err := LoadRotatingMasterKeyProvider(cfg.EncryptionRequired)
	if err != nil {
		return nil, err
	}
	if cfg.EncryptionRequired && (keys == nil || keys.CurrentKeyID() == "") {
		return nil, fmt.Errorf("state encryption required but master key unavailable")
	}

	var backend Backend
	switch {
	case cfg.PostgresDSN != "":
		backend, err = OpenPostgres(PostgresConfig{
			DSN:                    cfg.PostgresDSN,
			MaxReservedBytes:       cfg.MaxReservedBytes,
			EmergencyReserveBytes:  cfg.EmergencyReserveBytes,
			TenantMaxReservedBytes: cfg.TenantMaxReservedBytes,
			StateEpoch:             cfg.StateEpoch,
		})
	default:
		backend, err = OpenSQLite(SQLiteConfig{
			Path:                   cfg.SQLitePath,
			MaxReservedBytes:       cfg.MaxReservedBytes,
			EmergencyReserveBytes:  cfg.EmergencyReserveBytes,
			TenantMaxReservedBytes: cfg.TenantMaxReservedBytes,
			StateEpoch:             cfg.StateEpoch,
		})
	}
	if err != nil {
		return nil, err
	}

	rt := &Runtime{
		Backend:  backend,
		Coord:    NewCoordinator(backend),
		Keys:     keys,
		Socket:   cfg.Socket,
		Instance: cfg.InstanceID,
		Epoch:    cfg.StateEpoch,
		Backoff:  DefaultRestartBackoff(),
		leaseTTL: cfg.LeaseTTL,
	}

	if cfg.BlobRoot != "" {
		if keys == nil {
			_ = backend.Close()
			return nil, fmt.Errorf("blob store requires CLIPROXY_STATE_MASTER_KEY")
		}
		blobs, blobErr := NewBlobStore(cfg.BlobRoot, keys)
		if blobErr != nil {
			_ = backend.Close()
			return nil, blobErr
		}
		rt.Blobs = blobs
	}

	lease, err := backend.AcquireLease(ctx, LeasePayload{
		InstanceID:      cfg.InstanceID,
		BinaryVersion:   cfg.BinaryVersion,
		StateEpoch:      cfg.StateEpoch,
		TTLMilliseconds: cfg.LeaseTTL.Milliseconds(),
	})
	if err != nil {
		_ = backend.Close()
		return nil, fmt.Errorf("acquire writer lease: %w", err)
	}
	rt.Lease = lease

	srv, err := ListenUnix(rt.Coord, cfg.Socket)
	if err != nil {
		_ = backend.ReleaseLease(ctx, cfg.InstanceID)
		_ = backend.Close()
		return nil, err
	}
	rt.Server = srv

	hbCtx, cancel := context.WithCancel(context.Background())
	rt.stopHB = cancel
	rt.hbWG.Add(1)
	go rt.renewLoop(hbCtx)
	return rt, nil
}

func (rt *Runtime) renewLoop(ctx context.Context) {
	defer rt.hbWG.Done()
	interval := rt.leaseTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.mu.Lock()
			lease := rt.Lease
			backend := rt.Backend
			ttl := rt.leaseTTL
			rt.mu.Unlock()
			if lease == nil || backend == nil {
				continue
			}
			renewed, err := backend.RenewLease(ctx, LeasePayload{
				InstanceID:      lease.InstanceID,
				BinaryVersion:   lease.BinaryVersion,
				StateEpoch:      lease.StateEpoch,
				FencingGen:      lease.FencingGeneration,
				TTLMilliseconds: ttl.Milliseconds(),
			})
			if err != nil {
				// Never keep serving mutations after our durable writer authority
				// expires. Closing the UDS makes bridge operations fail closed and
				// degrades /readyz so the supervisor can restart cleanly. A
				// transient renewal failure before expiry keeps the valid lease.
				if !lease.ExpiresAt.IsZero() && !time.Now().UTC().Before(lease.ExpiresAt) {
					rt.mu.Lock()
					server := rt.Server
					rt.Server = nil
					rt.Lease = nil
					rt.mu.Unlock()
					if server != nil {
						_ = server.Close()
					}
					return
				}
				continue
			}
			rt.mu.Lock()
			rt.Lease = renewed
			rt.mu.Unlock()
		}
	}
}

// MigrationReady reports whether drain/migration gates allow traffic.
func (rt *Runtime) MigrationReady(ctx context.Context) (*MigrationSnapshot, error) {
	if rt == nil || rt.Coord == nil {
		return nil, fmt.Errorf("durable runtime unavailable")
	}
	return rt.Coord.SnapshotState(ctx)
}

// Close releases the lease, stops the socket server, and closes the backend.
func (rt *Runtime) Close(ctx context.Context) error {
	if rt == nil {
		return nil
	}
	if rt.stopHB != nil {
		rt.stopHB()
		rt.hbWG.Wait()
	}
	rt.mu.Lock()
	server := rt.Server
	lease := rt.Lease
	rt.Server = nil
	rt.Lease = nil
	rt.mu.Unlock()
	var first error
	if server != nil {
		if err := server.Close(); err != nil && first == nil {
			first = err
		}
	}
	if rt.Backend != nil && rt.Instance != "" && lease != nil {
		if err := rt.Backend.ReleaseLease(ctx, rt.Instance); err != nil && first == nil {
			first = err
		}
	}
	if rt.Backend != nil {
		if err := rt.Backend.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
