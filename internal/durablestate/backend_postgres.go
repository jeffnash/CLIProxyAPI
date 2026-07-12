package durablestate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// PostgresBackend is the horizontal/network-volume durable-state store.
type PostgresBackend struct {
	db               *sql.DB
	maxBytes         int64
	emergencyReserve int64
	tenantMaxBytes   int64
	stateEpoch       int64
	now              func() time.Time
	advisoryKey      int64
}

var _ Backend = (*PostgresBackend)(nil)

// PostgresConfig configures a PostgreSQL backend.
type PostgresConfig struct {
	DSN                    string
	MaxReservedBytes       int64
	EmergencyReserveBytes  int64
	TenantMaxReservedBytes int64
	StateEpoch             int64
}

// OpenPostgres opens a PostgreSQL durable-state backend.
func OpenPostgres(cfg PostgresConfig) (*PostgresBackend, error) {
	if cfg.DSN == "" {
		return nil, errors.New("postgres DSN required")
	}
	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxLifetime(5 * time.Minute)
	b := &PostgresBackend{
		db:               db,
		maxBytes:         cfg.MaxReservedBytes,
		emergencyReserve: cfg.EmergencyReserveBytes,
		tenantMaxBytes:   cfg.TenantMaxReservedBytes,
		now:              time.Now,
		stateEpoch:       cfg.StateEpoch,
		advisoryKey:      durableAdvisoryKey(),
	}
	if b.maxBytes <= 0 {
		b.maxBytes = 8 << 30
	}
	if b.emergencyReserve < 0 {
		b.emergencyReserve = 0
	}
	if b.emergencyReserve >= b.maxBytes {
		b.emergencyReserve = b.maxBytes / 8
	}
	if b.stateEpoch <= 0 {
		b.stateEpoch = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	if err := b.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

func durableAdvisoryKey() int64 {
	sum := sha256.Sum256([]byte("cliproxy-durable-state-writer-v1"))
	return int64(binary.BigEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
}

func (b *PostgresBackend) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS invocations (
  invocation_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL DEFAULT '',
  conversation_id TEXT NOT NULL DEFAULT '',
  client_idempotency_key TEXT NOT NULL DEFAULT '',
  canonical_request_hash TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL,
  evidence TEXT NOT NULL DEFAULT '',
  terminal_reason TEXT NOT NULL DEFAULT '',
  auth_id TEXT NOT NULL DEFAULT '',
  envelope_digest TEXT NOT NULL DEFAULT '',
  envelope_blob_ref TEXT NOT NULL DEFAULT '',
  journal_cursor BIGINT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  phase_updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS reservations (
  id TEXT PRIMARY KEY,
  invocation_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL DEFAULT '',
  bytes BIGINT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS reservations_tenant_idx ON reservations(tenant_id);
CREATE TABLE IF NOT EXISTS journal_events (
  invocation_id TEXT NOT NULL,
  sequence BIGINT NOT NULL,
  type TEXT NOT NULL,
  payload BYTEA,
  payload_digest TEXT NOT NULL DEFAULT '',
  acceptance_phase TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (invocation_id, sequence)
);
CREATE TABLE IF NOT EXISTS writer_lease (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  instance_id TEXT NOT NULL,
  binary_version TEXT NOT NULL,
  state_epoch BIGINT NOT NULL,
  fencing_generation BIGINT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);
`
	if _, err := b.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	_, err := b.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('state_epoch', $1)
ON CONFLICT (key) DO NOTHING`, fmt.Sprintf("%d", b.stateEpoch))
	return err
}

func (b *PostgresBackend) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	return b.db.Close()
}

func (b *PostgresBackend) Ping(ctx context.Context) error {
	return b.db.PingContext(ctx)
}

func (b *PostgresBackend) StateEpoch(ctx context.Context) (int64, error) {
	var raw string
	err := b.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='state_epoch'`).Scan(&raw)
	if err != nil {
		return b.stateEpoch, err
	}
	var epoch int64
	_, err = fmt.Sscanf(raw, "%d", &epoch)
	if err != nil {
		return b.stateEpoch, err
	}
	return epoch, nil
}

func (b *PostgresBackend) SetStateEpoch(ctx context.Context, epoch int64) error {
	if epoch <= 0 {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "epoch must be positive"}
	}
	current, err := b.StateEpoch(ctx)
	if err != nil {
		return err
	}
	if epoch <= current {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "epoch must advance"}
	}
	_, err = b.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('state_epoch', $1)
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, fmt.Sprintf("%d", epoch))
	if err != nil {
		return err
	}
	b.stateEpoch = epoch
	return nil
}

func (b *PostgresBackend) RequireWritableEpoch(callerEpoch int64) error {
	if callerEpoch <= 0 {
		callerEpoch = b.stateEpoch
	}
	if b.stateEpoch > callerEpoch {
		return &PhaseTransitionError{Code: CodeConflict, Message: "refusing to write unsupported older state epoch"}
	}
	return nil
}

func (b *PostgresBackend) GetInvocation(ctx context.Context, invocationID string) (*InvocationRecord, error) {
	row := b.db.QueryRowContext(ctx, `
SELECT invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
       phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
       created_at, updated_at, phase_updated_at
FROM invocations WHERE invocation_id=$1`, invocationID)
	rec, err := scanInvocationPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
	}
	return rec, err
}

func (b *PostgresBackend) ListInvocationsByPhases(ctx context.Context, phases []cliproxyexecutor.AcceptancePhase, limit int) ([]InvocationRecord, error) {
	if len(phases) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 256
	}
	args := make([]any, 0, len(phases)+1)
	placeholders := make([]string, len(phases))
	for i, phase := range phases {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args = append(args, string(phase))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`
SELECT invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
       phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
       created_at, updated_at, phase_updated_at
FROM invocations
WHERE phase IN (%s)
ORDER BY updated_at ASC
LIMIT $%d`, joinComma(placeholders), len(phases)+1)
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]InvocationRecord, 0, limit)
	for rows.Next() {
		rec, err := scanInvocationPG(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func (b *PostgresBackend) PutInvocation(ctx context.Context, rec *InvocationRecord) error {
	if rec == nil || rec.InvocationID == "" {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation required"}
	}
	now := b.now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	if rec.PhaseUpdatedAt.IsZero() {
		rec.PhaseUpdatedAt = now
	}
	_, err := b.db.ExecContext(ctx, `
INSERT INTO invocations(
  invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
  phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
  created_at, updated_at, phase_updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (invocation_id) DO NOTHING`,
		rec.InvocationID, rec.TenantID, rec.ConversationID, rec.ClientIdempotencyKey, rec.CanonicalRequestHash,
		string(rec.Phase), string(rec.Evidence), string(rec.TerminalReason), rec.AuthID, rec.EnvelopeDigest, rec.EnvelopeBlobRef, rec.JournalCursor,
		rec.CreatedAt, rec.UpdatedAt, rec.PhaseUpdatedAt)
	return err
}

func (b *PostgresBackend) DeleteInvocation(ctx context.Context, invocationID string) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reservations WHERE invocation_id=$1`, invocationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM journal_events WHERE invocation_id=$1`, invocationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM invocations WHERE invocation_id=$1`, invocationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *PostgresBackend) ClearEnvelope(ctx context.Context, invocationID string) error {
	res, err := b.db.ExecContext(ctx, `
UPDATE invocations SET envelope_blob_ref='', updated_at=$1
WHERE invocation_id=$2 AND phase=$3`, b.now().UTC(), invocationID, string(cliproxyexecutor.AcceptanceCompleted))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &PhaseTransitionError{Code: CodeConflict, Message: "clear envelope requires completed invocation"}
	}
	return nil
}

func (b *PostgresBackend) TransitionPhase(ctx context.Context, payload TransitionPhasePayload) (*InvocationRecord, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
SELECT invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
       phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
       created_at, updated_at, phase_updated_at
FROM invocations WHERE invocation_id=$1`, payload.InvocationID)
	rec, err := scanInvocationPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		if !payload.CreateIfMissing {
			return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
		}
		now := b.now().UTC()
		rec = &InvocationRecord{
			InvocationID:         payload.InvocationID,
			TenantID:             payload.TenantID,
			ConversationID:       payload.ConversationID,
			ClientIdempotencyKey: payload.ClientIdempotencyKey,
			CanonicalRequestHash: payload.CanonicalRequestHash,
			Phase:                cliproxyexecutor.AcceptanceNotSent,
			CreatedAt:            now,
			UpdatedAt:            now,
			PhaseUpdatedAt:       now,
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO invocations(
  invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
  phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
  created_at, updated_at, phase_updated_at
) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			rec.InvocationID, rec.TenantID, rec.ConversationID, rec.ClientIdempotencyKey, rec.CanonicalRequestHash,
			string(rec.Phase), string(rec.Evidence), string(rec.TerminalReason), rec.AuthID, rec.EnvelopeDigest, rec.EnvelopeBlobRef, rec.JournalCursor,
			rec.CreatedAt, rec.UpdatedAt, rec.PhaseUpdatedAt); err != nil {
			return nil, err
		}
		if payload.To == "" || payload.To == cliproxyexecutor.AcceptanceNotSent {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return rec, nil
		}
	} else if err != nil {
		return nil, err
	}
	if err := rejectInvocationIdentityReuse(rec, payload.TenantID, payload.ConversationID, payload.ClientIdempotencyKey, payload.CanonicalRequestHash); err != nil {
		return nil, err
	}
	from := rec.Phase
	if payload.From != "" && payload.From != from {
		return nil, &PhaseTransitionError{
			Code: CodeConflict, Message: fmt.Sprintf("expected phase %q, current %q", payload.From, from),
			From: payload.From, To: payload.To, Current: from,
		}
	}
	if !cliproxyexecutor.CanTransitionAcceptance(from, payload.To) {
		return nil, &PhaseTransitionError{
			Code: CodeIllegalTransition, Message: fmt.Sprintf("illegal acceptance transition %q -> %q", from, payload.To),
			From: from, To: payload.To, Current: from,
		}
	}
	now := b.now().UTC()
	rec.Phase = payload.To
	if payload.Evidence != "" {
		rec.Evidence = payload.Evidence
	}
	if payload.TerminalReason != "" {
		rec.TerminalReason = payload.TerminalReason
	}
	if payload.AuthID != "" {
		rec.AuthID = payload.AuthID
	}
	if payload.EnvelopeDigest != "" {
		rec.EnvelopeDigest = payload.EnvelopeDigest
	}
	if payload.EnvelopeBlobRef != "" {
		rec.EnvelopeBlobRef = payload.EnvelopeBlobRef
	}
	rec.UpdatedAt = now
	rec.PhaseUpdatedAt = now
	_, err = tx.ExecContext(ctx, `
UPDATE invocations SET
  phase=$1, evidence=$2, terminal_reason=$3, auth_id=$4, envelope_digest=$5, envelope_blob_ref=$6,
  updated_at=$7, phase_updated_at=$8
WHERE invocation_id=$9`,
		string(rec.Phase), string(rec.Evidence), string(rec.TerminalReason), rec.AuthID, rec.EnvelopeDigest, rec.EnvelopeBlobRef,
		rec.UpdatedAt, rec.PhaseUpdatedAt, rec.InvocationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rec, nil
}

func (b *PostgresBackend) Reserve(ctx context.Context, payload ReservePayload) (*ReservationRecord, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	priority := NormalizeAdmissionPriority(payload.Priority)
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reservations`).Scan(&used); err != nil {
		return nil, err
	}
	avail := AvailableDurableBytes(used, b.maxBytes, b.emergencyReserve, priority)
	if payload.Bytes > avail {
		return nil, &CapacityError{Message: "durable capacity exhausted", RetryAfter: 2 * time.Second}
	}
	rec := &ReservationRecord{
		ID: uuid.NewString(), InvocationID: payload.InvocationID, TenantID: payload.TenantID,
		Bytes: payload.Bytes, Priority: priority, Kind: payload.Kind, CreatedAt: b.now().UTC(),
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO reservations(id, invocation_id, tenant_id, bytes, priority, kind, created_at)
VALUES($1,$2,$3,$4,$5,$6,$7)`, rec.ID, rec.InvocationID, rec.TenantID, rec.Bytes, rec.Priority, rec.Kind, rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rec, nil
}

func (b *PostgresBackend) ResizeReservation(ctx context.Context, reservationID string, exactBytes int64, priority int) (*ReservationRecord, error) {
	if reservationID == "" || exactBytes <= 0 {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "reservation resize requires id and positive bytes"}
	}
	res, err := b.db.ExecContext(ctx, `UPDATE reservations SET bytes=$1, priority=$2 WHERE id=$3`, exactBytes, NormalizeAdmissionPriority(priority), reservationID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "reservation not found"}
	}
	return &ReservationRecord{ID: reservationID, Bytes: exactBytes, Priority: NormalizeAdmissionPriority(priority)}, nil
}

func (b *PostgresBackend) ReleaseReservation(ctx context.Context, reservationID string) error {
	res, err := b.db.ExecContext(ctx, `DELETE FROM reservations WHERE id=$1`, reservationID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &PhaseTransitionError{Code: CodeNotFound, Message: "reservation not found"}
	}
	return nil
}

func (b *PostgresBackend) ReservationBytes(ctx context.Context, tenantID string) (int64, error) {
	var used int64
	var err error
	if tenantID == "" {
		err = b.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reservations`).Scan(&used)
	} else {
		err = b.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reservations WHERE tenant_id=$1`, tenantID).Scan(&used)
	}
	return used, err
}

func (b *PostgresBackend) AppendJournal(ctx context.Context, event JournalEvent) (*JournalEvent, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var cursor int64
	err = tx.QueryRowContext(ctx, `SELECT journal_cursor FROM invocations WHERE invocation_id=$1`, event.InvocationID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
	}
	if err != nil {
		return nil, err
	}
	if event.Sequence == 0 {
		event.Sequence = cursor + 1
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = b.now().UTC()
	}
	payload := []byte(event.Payload)
	sum := sha256.Sum256(payload)
	computed := hex.EncodeToString(sum[:])
	if event.PayloadDigest == "" {
		event.PayloadDigest = computed
	} else if event.PayloadDigest != computed {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "journal payload digest mismatch"}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO journal_events(invocation_id, sequence, type, payload, payload_digest, acceptance_phase, created_at)
VALUES($1,$2,$3,$4,$5,$6,$7)`,
		event.InvocationID, event.Sequence, event.Type, payload, event.PayloadDigest, string(event.AcceptancePhase), event.CreatedAt)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE invocations SET journal_cursor=$1, updated_at=$2 WHERE invocation_id=$3`,
		event.Sequence, b.now().UTC(), event.InvocationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &event, nil
}

func (b *PostgresBackend) ReadJournal(ctx context.Context, invocationID string, fromSequence int64, limit int) ([]JournalEvent, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := b.db.QueryContext(ctx, `
SELECT invocation_id, sequence, type, payload, payload_digest, acceptance_phase, created_at
FROM journal_events
WHERE invocation_id=$1 AND sequence>$2
ORDER BY sequence ASC
LIMIT $3`, invocationID, fromSequence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]JournalEvent, 0, limit)
	for rows.Next() {
		var ev JournalEvent
		var payload []byte
		var phase string
		if err := rows.Scan(&ev.InvocationID, &ev.Sequence, &ev.Type, &payload, &ev.PayloadDigest, &phase, &ev.CreatedAt); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			ev.Payload = json.RawMessage(payload)
		}
		ev.AcceptancePhase = cliproxyexecutor.AcceptancePhase(phase)
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (b *PostgresBackend) AcquireLease(ctx context.Context, payload LeasePayload) (*LeaseRecord, error) {
	if payload.InstanceID == "" {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "instance_id required"}
	}
	epoch := payload.StateEpoch
	if epoch <= 0 {
		epoch = b.stateEpoch
	}
	if err := b.RequireWritableEpoch(epoch); err != nil {
		return nil, err
	}
	if storeEpoch, err := b.StateEpoch(ctx); err == nil && storeEpoch > epoch {
		return nil, &PhaseTransitionError{Code: CodeConflict, Message: "refusing to write unsupported older state epoch"}
	}
	ttl := time.Duration(payload.TTLMilliseconds) * time.Millisecond
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	now := b.now().UTC()
	expires := now.Add(ttl)

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, b.advisoryKey).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "postgres advisory writer lock unavailable"}
	}

	var existing LeaseRecord
	err = tx.QueryRowContext(ctx, `
SELECT instance_id, binary_version, state_epoch, fencing_generation, expires_at, updated_at
FROM writer_lease WHERE id=1`).Scan(
		&existing.InstanceID, &existing.BinaryVersion, &existing.StateEpoch, &existing.FencingGeneration, &existing.ExpiresAt, &existing.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		lease := &LeaseRecord{
			InstanceID: payload.InstanceID, BinaryVersion: payload.BinaryVersion,
			StateEpoch: epoch, FencingGeneration: 1, ExpiresAt: expires, UpdatedAt: now,
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO writer_lease(id, instance_id, binary_version, state_epoch, fencing_generation, expires_at, updated_at)
VALUES(1,$1,$2,$3,$4,$5,$6)`, lease.InstanceID, lease.BinaryVersion, lease.StateEpoch, lease.FencingGeneration, lease.ExpiresAt, lease.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return lease, nil
	case err != nil:
		return nil, err
	}
	live := existing.ExpiresAt.After(now)
	if existing.InstanceID != payload.InstanceID && live {
		return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "writer lease held by another instance"}
	}
	if existing.StateEpoch > epoch {
		return nil, &PhaseTransitionError{Code: CodeConflict, Message: "refusing to write unsupported older state epoch"}
	}
	if live && existing.InstanceID == payload.InstanceID {
		if payload.FencingGen <= 0 || payload.FencingGen != existing.FencingGeneration {
			return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "stale fencing generation"}
		}
	}
	lease := &LeaseRecord{
		InstanceID: payload.InstanceID, BinaryVersion: payload.BinaryVersion,
		StateEpoch: epoch, FencingGeneration: existing.FencingGeneration + 1,
		ExpiresAt: expires, UpdatedAt: now,
	}
	_, err = tx.ExecContext(ctx, `
UPDATE writer_lease SET instance_id=$1, binary_version=$2, state_epoch=$3, fencing_generation=$4, expires_at=$5, updated_at=$6
WHERE id=1`, lease.InstanceID, lease.BinaryVersion, lease.StateEpoch, lease.FencingGeneration, lease.ExpiresAt, lease.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lease, nil
}

func (b *PostgresBackend) RenewLease(ctx context.Context, payload LeasePayload) (*LeaseRecord, error) {
	if payload.InstanceID == "" || payload.FencingGen <= 0 {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "instance_id and fencing_generation required"}
	}
	ttl := time.Duration(payload.TTLMilliseconds) * time.Millisecond
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	now := b.now().UTC()
	res, err := b.db.ExecContext(ctx, `
UPDATE writer_lease SET expires_at=$1, updated_at=$2, binary_version=$3
WHERE id=1 AND instance_id=$4 AND fencing_generation=$5`,
		now.Add(ttl), now, payload.BinaryVersion, payload.InstanceID, payload.FencingGen)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "writer lease renew failed"}
	}
	return b.CurrentLease(ctx)
}

func (b *PostgresBackend) ReleaseLease(ctx context.Context, instanceID string) error {
	_, err := b.db.ExecContext(ctx, `DELETE FROM writer_lease WHERE id=1 AND instance_id=$1`, instanceID)
	return err
}

func (b *PostgresBackend) CurrentLease(ctx context.Context) (*LeaseRecord, error) {
	var lease LeaseRecord
	err := b.db.QueryRowContext(ctx, `
SELECT instance_id, binary_version, state_epoch, fencing_generation, expires_at, updated_at
FROM writer_lease WHERE id=1`).Scan(
		&lease.InstanceID, &lease.BinaryVersion, &lease.StateEpoch, &lease.FencingGeneration, &lease.ExpiresAt, &lease.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "no writer lease"}
	}
	return &lease, err
}

func scanInvocationPG(row scannable) (*InvocationRecord, error) {
	var rec InvocationRecord
	var phase, evidence, terminal string
	err := row.Scan(
		&rec.InvocationID, &rec.TenantID, &rec.ConversationID, &rec.ClientIdempotencyKey, &rec.CanonicalRequestHash,
		&phase, &evidence, &terminal, &rec.AuthID, &rec.EnvelopeDigest, &rec.EnvelopeBlobRef, &rec.JournalCursor,
		&rec.CreatedAt, &rec.UpdatedAt, &rec.PhaseUpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	rec.Phase = cliproxyexecutor.AcceptancePhase(phase)
	rec.Evidence = cliproxyexecutor.EvidenceCode(evidence)
	rec.TerminalReason = cliproxyexecutor.TerminalReason(terminal)
	return &rec, nil
}
