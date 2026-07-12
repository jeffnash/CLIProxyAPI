package durablestate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	_ "modernc.org/sqlite"
)

// SQLiteBackend is the single-writer/local-volume durable-state store.
type SQLiteBackend struct {
	db               *sql.DB
	maxBytes         int64
	emergencyReserve int64
	tenantMaxBytes   int64
	now              func() time.Time
	stateEpoch       int64
	path             string
}

// SQLiteConfig configures a SQLite WAL backend.
type SQLiteConfig struct {
	Path                   string
	MaxReservedBytes       int64
	EmergencyReserveBytes  int64 // withheld from non-recovery admissions
	TenantMaxReservedBytes int64 // 0 = unlimited per tenant
	StateEpoch             int64
}

// OpenSQLite opens (or creates) a SQLite WAL database for durable state.
func OpenSQLite(cfg SQLiteConfig) (*SQLiteBackend, error) {
	if cfg.Path == "" {
		return nil, errors.New("sqlite path required")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir durable state: %w", err)
	}
	dsn := cfg.Path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // exclusive writer for SQLite installations
	b := &SQLiteBackend{
		db:               db,
		maxBytes:         cfg.MaxReservedBytes,
		emergencyReserve: cfg.EmergencyReserveBytes,
		tenantMaxBytes:   cfg.TenantMaxReservedBytes,
		now:              time.Now,
		stateEpoch:       cfg.StateEpoch,
		path:             cfg.Path,
	}
	if b.maxBytes <= 0 {
		b.maxBytes = 8 << 30 // 8 GiB default durable budget
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

	if err := b.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return b, nil
}

func (b *SQLiteBackend) migrate(ctx context.Context) error {
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
  journal_cursor INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  phase_updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reservations (
  id TEXT PRIMARY KEY,
  invocation_id TEXT NOT NULL,
  tenant_id TEXT NOT NULL DEFAULT '',
  bytes INTEGER NOT NULL,
  priority INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS reservations_tenant_idx ON reservations(tenant_id);
CREATE TABLE IF NOT EXISTS journal_events (
  invocation_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  type TEXT NOT NULL,
  payload BLOB,
  payload_digest TEXT NOT NULL DEFAULT '',
  acceptance_phase TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (invocation_id, sequence)
);
CREATE TABLE IF NOT EXISTS writer_lease (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  instance_id TEXT NOT NULL,
  binary_version TEXT NOT NULL,
  state_epoch INTEGER NOT NULL,
  fencing_generation INTEGER NOT NULL,
  expires_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`
	if _, err := b.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	_, err := b.db.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key, value) VALUES('state_epoch', ?)`, fmt.Sprintf("%d", b.stateEpoch))
	return err
}

func (b *SQLiteBackend) Close() error {
	if b == nil || b.db == nil {
		return nil
	}
	return b.db.Close()
}

func (b *SQLiteBackend) Ping(ctx context.Context) error {
	return b.db.PingContext(ctx)
}

func (b *SQLiteBackend) StateEpoch(ctx context.Context) (int64, error) {
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

func (b *SQLiteBackend) SetStateEpoch(ctx context.Context, epoch int64) error {
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
	_, err = b.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('state_epoch', ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprintf("%d", epoch))
	if err != nil {
		return err
	}
	b.stateEpoch = epoch
	return nil
}

func (b *SQLiteBackend) CountReservations(ctx context.Context) (int64, error) {
	var n int64
	err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reservations`).Scan(&n)
	return n, err
}

func (b *SQLiteBackend) CountJournalEvents(ctx context.Context) (int64, error) {
	var n int64
	err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal_events`).Scan(&n)
	return n, err
}

// RequireWritableEpoch refuses mutations when the caller's epoch is behind the store.
func (b *SQLiteBackend) RequireWritableEpoch(callerEpoch int64) error {
	if callerEpoch <= 0 {
		callerEpoch = b.stateEpoch
	}
	if b.stateEpoch > callerEpoch {
		return &PhaseTransitionError{Code: CodeConflict, Message: "refusing to write unsupported older state epoch"}
	}
	return nil
}

func (b *SQLiteBackend) GetInvocation(ctx context.Context, invocationID string) (*InvocationRecord, error) {
	row := b.db.QueryRowContext(ctx, `
SELECT invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
       phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
       created_at, updated_at, phase_updated_at
FROM invocations WHERE invocation_id = ?`, invocationID)
	rec, err := scanInvocation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
	}
	return rec, err
}

func (b *SQLiteBackend) ListInvocationsByPhases(ctx context.Context, phases []cliproxyexecutor.AcceptancePhase, limit int) ([]InvocationRecord, error) {
	if len(phases) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 256
	}
	placeholders := make([]string, len(phases))
	args := make([]any, 0, len(phases)+1)
	for i, phase := range phases {
		placeholders[i] = "?"
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
LIMIT ?`, strings.Join(placeholders, ","))
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]InvocationRecord, 0, limit)
	for rows.Next() {
		rec, err := scanInvocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

func (b *SQLiteBackend) DeleteInvocation(ctx context.Context, invocationID string) error {
	if invocationID == "" {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation_id required"}
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reservations WHERE invocation_id=?`, invocationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM journal_events WHERE invocation_id=?`, invocationID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM invocations WHERE invocation_id=?`, invocationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *SQLiteBackend) ClearEnvelope(ctx context.Context, invocationID string) error {
	if invocationID == "" {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation_id required"}
	}
	now := b.now().UTC().Format(time.RFC3339Nano)
	res, err := b.db.ExecContext(ctx, `
UPDATE invocations SET envelope_blob_ref='', updated_at=?
WHERE invocation_id=? AND phase=?`, now, invocationID, string(cliproxyexecutor.AcceptanceCompleted))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &PhaseTransitionError{Code: CodeConflict, Message: "clear envelope requires completed invocation"}
	}
	return nil
}

func (b *SQLiteBackend) PutInvocation(ctx context.Context, rec *InvocationRecord) error {
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

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanInvocation(tx.QueryRowContext(ctx, `
SELECT invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
       phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
       created_at, updated_at, phase_updated_at
FROM invocations WHERE invocation_id = ?`, rec.InvocationID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := insertInvocationTx(tx, rec); err != nil {
			return err
		}
		return tx.Commit()
	case err != nil:
		return err
	}
	if err := rejectInvocationIdentityReuse(existing, rec.TenantID, rec.ConversationID, rec.ClientIdempotencyKey, rec.CanonicalRequestHash); err != nil {
		return err
	}
	// Identity matches: insert-only semantics — do not smash phase/terminal/envelope via upsert.
	return nil
}

func (b *SQLiteBackend) TransitionPhase(ctx context.Context, payload TransitionPhasePayload) (*InvocationRecord, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
SELECT invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
       phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
       created_at, updated_at, phase_updated_at
FROM invocations WHERE invocation_id = ?`, payload.InvocationID)
	rec, err := scanInvocation(row)
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
		if err := insertInvocationTx(tx, rec); err != nil {
			return nil, err
		}
		// Creating the record is itself the NOT_SENT commit. Callers may stop here
		// or continue into the first forward transition in the same request.
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
			Code:    CodeConflict,
			Message: fmt.Sprintf("expected phase %q, current %q", payload.From, from),
			From:    payload.From,
			To:      payload.To,
			Current: from,
		}
	}
	if !cliproxyexecutor.CanTransitionAcceptance(from, payload.To) {
		return nil, &PhaseTransitionError{
			Code:    CodeIllegalTransition,
			Message: fmt.Sprintf("illegal acceptance transition %q -> %q", from, payload.To),
			From:    from,
			To:      payload.To,
			Current: from,
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
  phase=?, evidence=?, terminal_reason=?, auth_id=?, envelope_digest=?, envelope_blob_ref=?,
  updated_at=?, phase_updated_at=?
WHERE invocation_id=?`,
		string(rec.Phase), string(rec.Evidence), string(rec.TerminalReason), rec.AuthID, rec.EnvelopeDigest, rec.EnvelopeBlobRef,
		rec.UpdatedAt.Format(time.RFC3339Nano), rec.PhaseUpdatedAt.Format(time.RFC3339Nano), rec.InvocationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rec, nil
}

func insertInvocationTx(tx *sql.Tx, rec *InvocationRecord) error {
	_, err := tx.Exec(`
INSERT INTO invocations(
  invocation_id, tenant_id, conversation_id, client_idempotency_key, canonical_request_hash,
  phase, evidence, terminal_reason, auth_id, envelope_digest, envelope_blob_ref, journal_cursor,
  created_at, updated_at, phase_updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.InvocationID, rec.TenantID, rec.ConversationID, rec.ClientIdempotencyKey, rec.CanonicalRequestHash,
		string(rec.Phase), string(rec.Evidence), string(rec.TerminalReason), rec.AuthID, rec.EnvelopeDigest, rec.EnvelopeBlobRef, rec.JournalCursor,
		rec.CreatedAt.Format(time.RFC3339Nano), rec.UpdatedAt.Format(time.RFC3339Nano), rec.PhaseUpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (b *SQLiteBackend) Reserve(ctx context.Context, payload ReservePayload) (*ReservationRecord, error) {
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
		return nil, &CapacityError{
			Message:    "durable capacity exhausted",
			RetryAfter: 2 * time.Second,
		}
	}
	if b.tenantMaxBytes > 0 && payload.TenantID != "" {
		var tenantUsed int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reservations WHERE tenant_id=?`, payload.TenantID).Scan(&tenantUsed); err != nil {
			return nil, err
		}
		if tenantUsed+payload.Bytes > b.tenantMaxBytes {
			return nil, &CapacityError{
				Message:    "tenant durable capacity exhausted",
				RetryAfter: 2 * time.Second,
			}
		}
	}
	rec := &ReservationRecord{
		ID:           uuid.NewString(),
		InvocationID: payload.InvocationID,
		TenantID:     payload.TenantID,
		Bytes:        payload.Bytes,
		Priority:     priority,
		Kind:         payload.Kind,
		CreatedAt:    b.now().UTC(),
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO reservations(id, invocation_id, tenant_id, bytes, priority, kind, created_at)
VALUES(?,?,?,?,?,?,?)`, rec.ID, rec.InvocationID, rec.TenantID, rec.Bytes, rec.Priority, rec.Kind, rec.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rec, nil
}

// ResizeReservation shrinks or grows an existing reservation to exactBytes.
// Growth is admitted with the same priority rules as Reserve.
func (b *SQLiteBackend) ResizeReservation(ctx context.Context, reservationID string, exactBytes int64, priority int) (*ReservationRecord, error) {
	if reservationID == "" || exactBytes <= 0 {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "reservation id and positive bytes required"}
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var rec ReservationRecord
	var created string
	err = tx.QueryRowContext(ctx, `
SELECT id, invocation_id, tenant_id, bytes, priority, kind, created_at
FROM reservations WHERE id=?`, reservationID).Scan(
		&rec.ID, &rec.InvocationID, &rec.TenantID, &rec.Bytes, &rec.Priority, &rec.Kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "reservation not found"}
	}
	if err != nil {
		return nil, err
	}
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	delta := exactBytes - rec.Bytes
	if delta > 0 {
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reservations`).Scan(&used); err != nil {
			return nil, err
		}
		// Exclude this reservation's current bytes from used for the growth check.
		used -= rec.Bytes
		prio := NormalizeAdmissionPriority(priority)
		if prio < rec.Priority {
			prio = rec.Priority
		}
		avail := AvailableDurableBytes(used, b.maxBytes, b.emergencyReserve, prio)
		if exactBytes > avail {
			return nil, &CapacityError{Message: "durable capacity exhausted", RetryAfter: 2 * time.Second}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE reservations SET bytes=? WHERE id=?`, exactBytes, reservationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rec.Bytes = exactBytes
	return &rec, nil
}

func (b *SQLiteBackend) ReleaseReservation(ctx context.Context, reservationID string) error {
	res, err := b.db.ExecContext(ctx, `DELETE FROM reservations WHERE id=?`, reservationID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &PhaseTransitionError{Code: CodeNotFound, Message: "reservation not found"}
	}
	return nil
}

func (b *SQLiteBackend) ReservationBytes(ctx context.Context, tenantID string) (int64, error) {
	var used int64
	var err error
	if tenantID == "" {
		err = b.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reservations`).Scan(&used)
	} else {
		err = b.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes),0) FROM reservations WHERE tenant_id=?`, tenantID).Scan(&used)
	}
	return used, err
}

func (b *SQLiteBackend) AppendJournal(ctx context.Context, event JournalEvent) (*JournalEvent, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var cursor int64
	var phaseRaw string
	err = tx.QueryRowContext(ctx, `SELECT journal_cursor, phase FROM invocations WHERE invocation_id=?`, event.InvocationID).Scan(&cursor, &phaseRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
	}
	if err != nil {
		return nil, err
	}
	next := cursor + 1
	if event.Sequence == 0 {
		event.Sequence = next
	}
	if event.Sequence != next {
		return nil, &PhaseTransitionError{
			Code:    CodeConflict,
			Message: fmt.Sprintf("expected journal sequence %d, got %d", next, event.Sequence),
		}
	}
	if err := validateJournalCommit(&event, cliproxyexecutor.AcceptancePhase(phaseRaw)); err != nil {
		return nil, err
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = b.now().UTC()
	}
	var payload []byte
	if len(event.Payload) > 0 {
		payload = []byte(event.Payload)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO journal_events(invocation_id, sequence, type, payload, payload_digest, acceptance_phase, created_at)
VALUES(?,?,?,?,?,?,?)`,
		event.InvocationID, event.Sequence, event.Type, payload, event.PayloadDigest, string(event.AcceptancePhase), event.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE invocations SET journal_cursor=?, updated_at=? WHERE invocation_id=?`,
		event.Sequence, b.now().UTC().Format(time.RFC3339Nano), event.InvocationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &event, nil
}

func (b *SQLiteBackend) ReadJournal(ctx context.Context, invocationID string, fromSequence int64, limit int) ([]JournalEvent, error) {
	rows, err := b.db.QueryContext(ctx, `
SELECT invocation_id, sequence, type, payload, payload_digest, acceptance_phase, created_at
FROM journal_events
WHERE invocation_id=? AND sequence > ?
ORDER BY sequence ASC
LIMIT ?`, invocationID, fromSequence, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]JournalEvent, 0, limit)
	for rows.Next() {
		var ev JournalEvent
		var payload []byte
		var phase string
		var created string
		if err := rows.Scan(&ev.InvocationID, &ev.Sequence, &ev.Type, &payload, &ev.PayloadDigest, &phase, &created); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			ev.Payload = json.RawMessage(payload)
		}
		ev.AcceptancePhase = cliproxyexecutor.AcceptancePhase(phase)
		ts, _ := time.Parse(time.RFC3339Nano, created)
		ev.CreatedAt = ts
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (b *SQLiteBackend) AcquireLease(ctx context.Context, payload LeasePayload) (*LeaseRecord, error) {
	if payload.InstanceID == "" {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "instance_id required"}
	}
	ttl := time.Duration(payload.TTLMilliseconds) * time.Millisecond
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	now := b.now().UTC()
	expires := now.Add(ttl)
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

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing LeaseRecord
	var expiresRaw, updatedRaw string
	err = tx.QueryRowContext(ctx, `
SELECT instance_id, binary_version, state_epoch, fencing_generation, expires_at, updated_at
FROM writer_lease WHERE id=1`).Scan(
		&existing.InstanceID, &existing.BinaryVersion, &existing.StateEpoch, &existing.FencingGeneration, &expiresRaw, &updatedRaw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		lease := &LeaseRecord{
			InstanceID:        payload.InstanceID,
			BinaryVersion:     payload.BinaryVersion,
			StateEpoch:        epoch,
			FencingGeneration: 1,
			ExpiresAt:         expires,
			UpdatedAt:         now,
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO writer_lease(id, instance_id, binary_version, state_epoch, fencing_generation, expires_at, updated_at)
VALUES(1,?,?,?,?,?,?)`, lease.InstanceID, lease.BinaryVersion, lease.StateEpoch, lease.FencingGeneration,
			lease.ExpiresAt.Format(time.RFC3339Nano), lease.UpdatedAt.Format(time.RFC3339Nano))
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

	existing.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresRaw)
	existing.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedRaw)
	live := existing.ExpiresAt.After(now)
	if existing.InstanceID != payload.InstanceID && live {
		return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "writer lease held by another instance"}
	}
	if existing.StateEpoch > epoch {
		return nil, &PhaseTransitionError{Code: CodeConflict, Message: "refusing to write unsupported older state epoch"}
	}
	// Same instance reclaiming a live lease must present the current fencing generation.
	if live && existing.InstanceID == payload.InstanceID {
		if payload.FencingGen <= 0 {
			return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "fencing_generation required to reacquire live lease"}
		}
		if payload.FencingGen != existing.FencingGeneration {
			return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "stale fencing generation"}
		}
	}
	lease := &LeaseRecord{
		InstanceID:        payload.InstanceID,
		BinaryVersion:     payload.BinaryVersion,
		StateEpoch:        epoch,
		FencingGeneration: existing.FencingGeneration + 1,
		ExpiresAt:         expires,
		UpdatedAt:         now,
	}
	_, err = tx.ExecContext(ctx, `
UPDATE writer_lease SET instance_id=?, binary_version=?, state_epoch=?, fencing_generation=?, expires_at=?, updated_at=?
WHERE id=1`, lease.InstanceID, lease.BinaryVersion, lease.StateEpoch, lease.FencingGeneration,
		lease.ExpiresAt.Format(time.RFC3339Nano), lease.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lease, nil
}

func (b *SQLiteBackend) RenewLease(ctx context.Context, payload LeasePayload) (*LeaseRecord, error) {
	if payload.InstanceID == "" {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "instance_id required"}
	}
	if payload.FencingGen <= 0 {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "fencing_generation required"}
	}
	ttl := time.Duration(payload.TTLMilliseconds) * time.Millisecond
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	now := b.now().UTC()
	expires := now.Add(ttl)
	epoch := payload.StateEpoch
	if epoch <= 0 {
		epoch = b.stateEpoch
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing LeaseRecord
	var expiresRaw, updatedRaw string
	err = tx.QueryRowContext(ctx, `
SELECT instance_id, binary_version, state_epoch, fencing_generation, expires_at, updated_at
FROM writer_lease WHERE id=1`).Scan(
		&existing.InstanceID, &existing.BinaryVersion, &existing.StateEpoch, &existing.FencingGeneration, &expiresRaw, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "no writer lease"}
	}
	if err != nil {
		return nil, err
	}
	existing.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresRaw)
	existing.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedRaw)
	if existing.InstanceID != payload.InstanceID {
		return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "cannot renew lease owned by another instance"}
	}
	if payload.FencingGen != existing.FencingGeneration {
		return nil, &PhaseTransitionError{Code: CodeLeaseLost, Message: "stale fencing generation"}
	}
	if existing.StateEpoch > epoch {
		return nil, &PhaseTransitionError{Code: CodeConflict, Message: "refusing to write unsupported older state epoch"}
	}
	lease := &LeaseRecord{
		InstanceID:        existing.InstanceID,
		BinaryVersion:     payload.BinaryVersion,
		StateEpoch:        epoch,
		FencingGeneration: existing.FencingGeneration,
		ExpiresAt:         expires,
		UpdatedAt:         now,
	}
	if lease.BinaryVersion == "" {
		lease.BinaryVersion = existing.BinaryVersion
	}
	_, err = tx.ExecContext(ctx, `
UPDATE writer_lease SET binary_version=?, state_epoch=?, expires_at=?, updated_at=?
WHERE id=1 AND instance_id=? AND fencing_generation=?`,
		lease.BinaryVersion, lease.StateEpoch, lease.ExpiresAt.Format(time.RFC3339Nano), lease.UpdatedAt.Format(time.RFC3339Nano),
		lease.InstanceID, lease.FencingGeneration)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lease, nil
}

func (b *SQLiteBackend) ReleaseLease(ctx context.Context, instanceID string) error {
	res, err := b.db.ExecContext(ctx, `DELETE FROM writer_lease WHERE id=1 AND instance_id=?`, instanceID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return &PhaseTransitionError{Code: CodeLeaseLost, Message: "lease not held"}
	}
	return nil
}

func (b *SQLiteBackend) CurrentLease(ctx context.Context) (*LeaseRecord, error) {
	var lease LeaseRecord
	var expiresRaw, updatedRaw string
	err := b.db.QueryRowContext(ctx, `
SELECT instance_id, binary_version, state_epoch, fencing_generation, expires_at, updated_at
FROM writer_lease WHERE id=1`).Scan(
		&lease.InstanceID, &lease.BinaryVersion, &lease.StateEpoch, &lease.FencingGeneration, &expiresRaw, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "no writer lease"}
	}
	if err != nil {
		return nil, err
	}
	lease.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expiresRaw)
	lease.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedRaw)
	return &lease, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanInvocation(row scannable) (*InvocationRecord, error) {
	var rec InvocationRecord
	var phase, evidence, terminal, created, updated, phaseUpdated string
	err := row.Scan(
		&rec.InvocationID, &rec.TenantID, &rec.ConversationID, &rec.ClientIdempotencyKey, &rec.CanonicalRequestHash,
		&phase, &evidence, &terminal, &rec.AuthID, &rec.EnvelopeDigest, &rec.EnvelopeBlobRef, &rec.JournalCursor,
		&created, &updated, &phaseUpdated,
	)
	if err != nil {
		return nil, err
	}
	rec.Phase = cliproxyexecutor.AcceptancePhase(phase)
	rec.Evidence = cliproxyexecutor.EvidenceCode(evidence)
	rec.TerminalReason = cliproxyexecutor.TerminalReason(terminal)
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	rec.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	rec.PhaseUpdatedAt, _ = time.Parse(time.RFC3339Nano, phaseUpdated)
	return &rec, nil
}

func rejectInvocationIdentityReuse(existing *InvocationRecord, tenantID, conversationID, idempotencyKey, requestHash string) error {
	if existing == nil {
		return nil
	}
	if tenantID != "" && existing.TenantID != "" && tenantID != existing.TenantID {
		return &PhaseTransitionError{Code: CodeConflict, Message: "invocation identity mismatch: tenant_id"}
	}
	if conversationID != "" && existing.ConversationID != "" && conversationID != existing.ConversationID {
		return &PhaseTransitionError{Code: CodeConflict, Message: "invocation identity mismatch: conversation_id"}
	}
	if idempotencyKey != "" && existing.ClientIdempotencyKey != "" && idempotencyKey != existing.ClientIdempotencyKey {
		return &PhaseTransitionError{Code: CodeConflict, Message: "invocation identity mismatch: client_idempotency_key"}
	}
	if requestHash != "" && existing.CanonicalRequestHash != "" && requestHash != existing.CanonicalRequestHash {
		return &PhaseTransitionError{Code: CodeConflict, Message: "invocation identity mismatch: canonical_request_hash"}
	}
	return nil
}

func validateJournalCommit(event *JournalEvent, currentPhase cliproxyexecutor.AcceptancePhase) error {
	if event == nil {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "journal event required"}
	}
	payload := []byte(event.Payload)
	digest := sha256.Sum256(payload)
	computed := hex.EncodeToString(digest[:])
	if event.PayloadDigest == "" {
		event.PayloadDigest = computed
	} else if event.PayloadDigest != computed {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "journal payload digest mismatch"}
	}
	if event.AcceptancePhase == "" {
		return nil
	}
	if !cliproxyexecutor.IsValidAcceptancePhase(event.AcceptancePhase) {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invalid journal acceptance_phase"}
	}
	if currentPhase != "" && event.AcceptancePhase != currentPhase {
		return &PhaseTransitionError{
			Code:    CodeConflict,
			Message: fmt.Sprintf("journal acceptance_phase %q disagrees with invocation phase %q", event.AcceptancePhase, currentPhase),
		}
	}
	return nil
}
