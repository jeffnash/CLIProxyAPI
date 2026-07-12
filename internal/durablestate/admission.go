package durablestate

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Admission priorities (higher wins under contention). Fresh turns are safest
// to queue/reject because they have not crossed the upstream send boundary.
const (
	AdmissionPriorityFresh            = 10
	AdmissionPriorityStreamResume     = 20
	AdmissionPriorityToolContinuation = 30
	AdmissionPriorityRecovery         = 40 // MAYBE_ACCEPTED / ACCEPTED recovery
)

// DefaultAdaptiveMemoryInitialBytes is the starting in-memory replay/journal tail.
const DefaultAdaptiveMemoryInitialBytes int64 = 2 << 20 // 2 MiB

// NextGeometricBytes returns the next reservation size that covers neededBytes.
// Growth is geometric from initial (default 2MiB) and never exceeds hardCap.
func NextGeometricBytes(current, needed, initial, hardCap int64) (int64, error) {
	if needed <= 0 {
		return current, nil
	}
	if hardCap <= 0 {
		return 0, fmt.Errorf("hard capacity cap required")
	}
	if needed > hardCap {
		return 0, &CapacityError{Message: "requested bytes exceed hard capacity cap", RetryAfter: 0}
	}
	if initial <= 0 {
		initial = DefaultAdaptiveMemoryInitialBytes
	}
	if initial > hardCap {
		initial = hardCap
	}
	next := current
	if next < initial {
		next = initial
	}
	for next < needed {
		doubled := next * 2
		if doubled <= next || doubled > hardCap {
			next = hardCap
			break
		}
		next = doubled
	}
	if next < needed {
		return 0, &CapacityError{Message: "geometric growth cannot cover requested bytes", RetryAfter: 0}
	}
	return next, nil
}

// AdmissionClassifies maps a priority integer onto the plan's admission ladder.
func NormalizeAdmissionPriority(priority int) int {
	switch {
	case priority >= AdmissionPriorityRecovery:
		return AdmissionPriorityRecovery
	case priority >= AdmissionPriorityToolContinuation:
		return AdmissionPriorityToolContinuation
	case priority >= AdmissionPriorityStreamResume:
		return AdmissionPriorityStreamResume
	case priority > 0:
		return AdmissionPriorityFresh
	default:
		return AdmissionPriorityFresh
	}
}

// AvailableDurableBytes reports how many bytes remain for the given priority.
// An emergency recovery slice is withheld from non-recovery admissions.
func AvailableDurableBytes(used, maxBytes, emergencyReserve int64, priority int) int64 {
	if maxBytes <= 0 {
		return 0
	}
	if used < 0 {
		used = 0
	}
	ceiling := maxBytes
	if NormalizeAdmissionPriority(priority) < AdmissionPriorityRecovery && emergencyReserve > 0 {
		ceiling = maxBytes - emergencyReserve
		if ceiling < 0 {
			ceiling = 0
		}
	}
	avail := ceiling - used
	if avail < 0 {
		return 0
	}
	return avail
}

// AdmissionGate controls fresh-turn admission during restart/migration drain.
type AdmissionGate struct {
	draining atomic.Bool
}

// BeginDrain stops fresh admissions while recovery/resume continue.
func (g *AdmissionGate) BeginDrain() {
	if g == nil {
		return
	}
	g.draining.Store(true)
}

// EndDrain restores fresh admissions after restart/migration settles.
func (g *AdmissionGate) EndDrain() {
	if g == nil {
		return
	}
	g.draining.Store(false)
}

// Draining reports whether fresh admissions are blocked.
func (g *AdmissionGate) Draining() bool {
	return g != nil && g.draining.Load()
}

// AllowAdmission rejects fresh turns while draining; recovery/resume/tool continue.
func (g *AdmissionGate) AllowAdmission(priority int) error {
	if g == nil || !g.draining.Load() {
		return nil
	}
	if NormalizeAdmissionPriority(priority) > AdmissionPriorityFresh {
		return nil
	}
	return &CapacityError{Message: "fresh admission drained for restart", RetryAfter: 5 * time.Second}
}
