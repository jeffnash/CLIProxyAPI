package turnprovenance

// resolveExplicit applies authoritative role and explicit provenance metadata.
// Returns ok=false when explicit evidence is insufficient or conflicting.
func resolveExplicit(segments []Segment) (Decision, bool) {
	var current, standing []string
	var evidence []Evidence
	var userDeclared int
	conflicts := false

	for _, seg := range segments {
		switch seg.Role {
		case RoleSystem, RoleDeveloper:
			standing = append(standing, seg.ID)
			evidence = append(evidence, Evidence{Class: EvidenceExplicit, Code: "role_standing", Segment: seg.ID})
			continue
		case RoleTool:
			// Tool results are continuations, not competing user intent.
			evidence = append(evidence, Evidence{Class: EvidenceExplicit, Code: "role_tool_continuation", Segment: seg.ID})
			continue
		case RoleAssistant:
			evidence = append(evidence, Evidence{Class: EvidenceExplicit, Code: "role_assistant", Segment: seg.ID})
			continue
		}

		switch seg.DeclaredProvenance {
		case ProvenanceCurrent:
			current = append(current, seg.ID)
			userDeclared++
			evidence = append(evidence, Evidence{Class: EvidenceExplicit, Code: "declared_current", Segment: seg.ID})
		case ProvenanceStanding:
			standing = append(standing, seg.ID)
			userDeclared++
			evidence = append(evidence, Evidence{Class: EvidenceExplicit, Code: "declared_standing", Segment: seg.ID})
		case ProvenanceFragment:
			userDeclared++
			evidence = append(evidence, Evidence{Class: EvidenceExplicit, Code: "declared_fragment", Segment: seg.ID})
		case ProvenanceUnspecified:
			// fall through to structural/recurrence ladders
		default:
			conflicts = true
			evidence = append(evidence, Evidence{Class: EvidenceAmbiguous, Code: "unknown_declaration", Segment: seg.ID, Detail: string(seg.DeclaredProvenance)})
		}
	}

	if conflicts {
		return Decision{
			Kind:       DecisionClarificationRequired,
			Evidence:   evidence,
			ReasonCode: ReasonConflictingDeclarations,
			Candidates: userCandidates(segments),
		}, true
	}

	// Explicit current+standing is enough only when every competing user is classified.
	// A declared current beside an unspecified adjacent user must not resolve.
	if len(current) >= 1 && userDeclared > 0 && !hasUnresolvedUserFragments(segments) && !hasUnclassifiedCompetingUsers(segments) {
		return Decision{
			Kind:             DecisionResolvedExplicit,
			CurrentSegments:  current,
			StandingSegments: standing,
			Evidence:         evidence,
			ReasonCode:       ReasonExplicitMetadata,
			Candidates:       userCandidates(segments),
		}, true
	}

	// Only system/developer/tool/assistant — no ambiguous user adjacency.
	if countTrailingAmbiguousUsers(segments) == 0 {
		if len(current) == 0 {
			// Last non-empty user (if any) is current by role ladder when unique.
			if ids := trailingUserIDs(segments); len(ids) == 1 {
				current = ids
				evidence = append(evidence, Evidence{Class: EvidenceExplicit, Code: "single_trailing_user", Segment: ids[0]})
				return Decision{
					Kind:             DecisionResolvedExplicit,
					CurrentSegments:  current,
					StandingSegments: standing,
					Evidence:         evidence,
					ReasonCode:       ReasonExplicitRoles,
					Candidates:       userCandidates(segments),
				}, true
			}
		}
	}
	return Decision{}, false
}

func hasUnresolvedUserFragments(segments []Segment) bool {
	for _, seg := range segments {
		if seg.Role == RoleUser && seg.DeclaredProvenance == ProvenanceFragment {
			return true
		}
	}
	return false
}

// hasUnclassifiedCompetingUsers reports trailing adjacent users that still lack
// an explicit provenance declaration.
func hasUnclassifiedCompetingUsers(segments []Segment) bool {
	for _, seg := range trailingUserSegments(segments) {
		if seg.Role == RoleUser && seg.DeclaredProvenance == ProvenanceUnspecified {
			return true
		}
	}
	return false
}

func countTrailingAmbiguousUsers(segments []Segment) int {
	users := trailingUserSegments(segments)
	n := 0
	for _, seg := range users {
		if seg.DeclaredProvenance == ProvenanceUnspecified && (seg.ByteLength > 0 || seg.HasImages) {
			n++
		}
	}
	return n
}

func trailingUserSegments(segments []Segment) []Segment {
	out := make([]Segment, 0, 2)
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if seg.Role != RoleUser {
			if len(out) > 0 {
				break
			}
			continue
		}
		out = append([]Segment{seg}, out...)
		if len(out) == 2 {
			break
		}
	}
	return out
}

func trailingUserIDs(segments []Segment) []string {
	users := trailingUserSegments(segments)
	ids := make([]string, 0, len(users))
	for _, seg := range users {
		if seg.ByteLength > 0 || seg.HasImages {
			ids = append(ids, seg.ID)
		}
	}
	return ids
}

func userCandidates(segments []Segment) []Segment {
	out := make([]Segment, 0, len(segments))
	for _, seg := range segments {
		if seg.Role == RoleUser {
			out = append(out, seg)
		}
	}
	return out
}
