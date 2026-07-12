package durablestate

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

// FeatureFlags are rollout controls (not permanent deferrals).
type FeatureFlags struct {
	ProvenanceShadow        bool
	ProvenanceEnforce       bool
	TypedClarification      bool
	AcceptancePhaseReceipts bool
	StateCoordinator        bool
	DurableLiveStreaming    bool
	AdaptiveReservations    bool
	EncryptedState          bool
	NewCompactor            bool
}

// LoadFeatureFlagsFromEnv reads CLIPROXY_FLAG_* environment variables.
func LoadFeatureFlagsFromEnv() FeatureFlags {
	return FeatureFlags{
		ProvenanceShadow:        envBool("CLIPROXY_FLAG_PROVENANCE_SHADOW", false),
		ProvenanceEnforce:       envBool("CLIPROXY_FLAG_PROVENANCE_ENFORCE", false),
		TypedClarification:      envBool("CLIPROXY_FLAG_TYPED_CLARIFICATION", true),
		AcceptancePhaseReceipts: envBool("CLIPROXY_FLAG_ACCEPTANCE_PHASE_RECEIPTS", true),
		StateCoordinator:        envBool("CLIPROXY_FLAG_STATE_COORDINATOR", true),
		DurableLiveStreaming:    envBool("CLIPROXY_FLAG_DURABLE_LIVE_STREAMING", false),
		AdaptiveReservations:    envBool("CLIPROXY_FLAG_ADAPTIVE_RESERVATIONS", true),
		EncryptedState:          envBool("CLIPROXY_FLAG_ENCRYPTED_STATE", false),
		NewCompactor:            envBool("CLIPROXY_FLAG_NEW_COMPACTOR", true),
	}
}

func envBool(key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return def
		}
		return v
	}
}

// Metrics holds process-local durable-state counters.
type Metrics struct {
	PhaseTransitions        atomic.Int64
	PhaseTransitionFailures atomic.Int64
	JournalAppends          atomic.Int64
	ReservationsGranted     atomic.Int64
	ReservationsRejected    atomic.Int64
	BlobPuts                atomic.Int64
	BlobDedupeHits          atomic.Int64
	EpochAdvances           atomic.Int64
	CASImports              atomic.Int64
	UnresolvedListed        atomic.Int64
}

// Snapshot returns a copy of current counter values.
func (m *Metrics) Snapshot() map[string]int64 {
	if m == nil {
		return map[string]int64{}
	}
	return map[string]int64{
		"phase_transitions":         m.PhaseTransitions.Load(),
		"phase_transition_failures": m.PhaseTransitionFailures.Load(),
		"journal_appends":           m.JournalAppends.Load(),
		"reservations_granted":      m.ReservationsGranted.Load(),
		"reservations_rejected":     m.ReservationsRejected.Load(),
		"blob_puts":                 m.BlobPuts.Load(),
		"blob_dedupe_hits":          m.BlobDedupeHits.Load(),
		"epoch_advances":            m.EpochAdvances.Load(),
		"cas_imports":               m.CASImports.Load(),
		"unresolved_listed":         m.UnresolvedListed.Load(),
	}
}

// DefaultMetrics is the package process counter set.
var DefaultMetrics Metrics
