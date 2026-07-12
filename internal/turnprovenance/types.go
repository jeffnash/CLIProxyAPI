// Package turnprovenance resolves which message segments are current-turn
// intent versus standing context using explicit metadata, structural fragment
// evidence, and conversation-scoped recurrence — never keywords or model calls.
package turnprovenance

import "time"

// Role is a provider-neutral message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Provenance is an explicit client-declared segment class.
type Provenance string

const (
	ProvenanceUnspecified Provenance = ""
	ProvenanceCurrent     Provenance = "current"
	ProvenanceStanding    Provenance = "standing"
	ProvenanceFragment    Provenance = "fragment"
)

// DecisionKind is the evidence class of a resolver decision.
type DecisionKind string

const (
	DecisionResolvedExplicit      DecisionKind = "resolved_explicit"
	DecisionResolvedFragments     DecisionKind = "resolved_fragments"
	DecisionResolvedRecurrence    DecisionKind = "resolved_recurrence"
	DecisionClarificationRequired DecisionKind = "clarification_required"
)

// EvidenceClass names how a decision was justified.
type EvidenceClass string

const (
	EvidenceExplicit   EvidenceClass = "explicit"
	EvidenceStructural EvidenceClass = "structural"
	EvidenceRecurrence EvidenceClass = "recurrence"
	EvidenceAmbiguous  EvidenceClass = "ambiguous"
)

// Segment is one normalized message segment under consideration.
type Segment struct {
	ID                 string
	OriginalIndex      int
	Role               Role
	ContentDigest      string
	ByteLength         int
	HasImages          bool
	TurnID             string
	FragmentGroupID    string
	FragmentIndex      *int
	FragmentCount      *int
	ContinuationOf     string
	DeclaredProvenance Provenance
	TextPreview        string
}

// Evidence records one supporting (or conflicting) observation.
type Evidence struct {
	Class   EvidenceClass `json:"class"`
	Code    string        `json:"code"`
	Detail  string        `json:"detail,omitempty"`
	Segment string        `json:"segment,omitempty"`
}

// Decision is the resolver output for one inbound turn.
type Decision struct {
	Kind             DecisionKind
	CurrentSegments  []string
	StandingSegments []string
	MergeGroups      [][]string
	Evidence         []Evidence
	ReasonCode       string
	Candidates       []Segment
}

// ClarificationRequired reports whether tools/providers must not run.
func (d Decision) ClarificationRequired() bool {
	return d.Kind == DecisionClarificationRequired
}

// Input is everything the resolver needs. Policy itself does not depend on
// provider or client names.
type Input struct {
	TenantID       string
	ConversationID string
	InvocationID   string
	Segments       []Segment
	ToolCapable    bool
	Recurrence     RecurrenceEvidence
	Now            time.Time
}

// RecurrenceEvidence is durable conversation-scoped recurrence data.
type RecurrenceEvidence struct {
	Enabled        bool
	MinOccurrences int
	MinByteLength  int
	Observations   []RecurrenceObservation
}

// RecurrenceObservation is one prior COMPLETED-turn observation of a segment fingerprint.
type RecurrenceObservation struct {
	SegmentDigest       string
	NeighborDigest      string
	RelativeSuffixIndex int
	Occurrences         int
	LastSeenTurn        int64
}

// Reason codes for decisions and clarifications.
const (
	ReasonExplicitRoles           = "explicit_roles"
	ReasonExplicitMetadata        = "explicit_metadata"
	ReasonCompleteFragments       = "complete_fragments"
	ReasonRecurringStanding       = "recurring_standing"
	ReasonAmbiguousAdjacentUsers  = "ambiguous_adjacent_users"
	ReasonIncompleteFragments     = "incomplete_fragments"
	ReasonConflictingDeclarations = "conflicting_declarations"
	ReasonColdStart               = "cold_start"
	ReasonNoStableConversation    = "no_stable_conversation"
)
