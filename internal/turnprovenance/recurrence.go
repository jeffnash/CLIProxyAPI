package turnprovenance

// resolveRecurrence promotes a stable standing user segment when durable
// COMPLETED-turn recurrence evidence is sufficient. Cold-start is conservative.
func resolveRecurrence(in Input, secret []byte) (Decision, bool) {
	if in.ConversationID == "" || in.TenantID == "" {
		return Decision{
			Kind:       DecisionClarificationRequired,
			ReasonCode: ReasonNoStableConversation,
			Candidates: userCandidates(in.Segments),
			Evidence: []Evidence{{
				Class: EvidenceAmbiguous,
				Code:  "no_stable_conversation",
			}},
		}, AmbiguousTrailingUsers(in.Segments)
	}
	if !in.Recurrence.Enabled {
		return Decision{}, false
	}

	minOcc := in.Recurrence.MinOccurrences
	if minOcc <= 0 {
		minOcc = 3
	}
	minBytes := in.Recurrence.MinByteLength
	if minBytes <= 0 {
		minBytes = 64
	}

	users := trailingUserSegments(in.Segments)
	if len(users) < 2 {
		return Decision{}, false
	}
	// Consider the earlier trailing user as standing candidate and the last as current.
	standingCand := users[len(users)-2]
	currentCand := users[len(users)-1]
	if standingCand.ByteLength < minBytes {
		return Decision{}, false
	}
	if standingCand.HasImages && standingCand.DeclaredProvenance != ProvenanceStanding {
		return Decision{
			Kind:       DecisionClarificationRequired,
			ReasonCode: ReasonAmbiguousAdjacentUsers,
			Candidates: userCandidates(in.Segments),
			Evidence: []Evidence{{
				Class:   EvidenceAmbiguous,
				Code:    "image_bearing_without_declaration",
				Segment: standingCand.ID,
			}},
		}, true
	}

	standingKey := SegmentFingerprint(secret, in.TenantID, in.ConversationID, standingCand.ContentDigest)
	obs := findObservation(in.Recurrence.Observations, standingKey)
	if obs == nil || obs.Occurrences < minOcc {
		return Decision{
			Kind:       DecisionClarificationRequired,
			ReasonCode: ReasonColdStart,
			Candidates: userCandidates(in.Segments),
			Evidence: []Evidence{{
				Class:   EvidenceAmbiguous,
				Code:    "recurrence_below_threshold",
				Segment: standingCand.ID,
			}},
		}, AmbiguousTrailingUsers(in.Segments)
	}
	// Neighboring current-intent fingerprint must change across those turns.
	currentKey := SegmentFingerprint(secret, in.TenantID, in.ConversationID, currentCand.ContentDigest)
	if obs.NeighborDigest == "" || obs.NeighborDigest == currentKey {
		// unchanged neighboring intent is insufficient alone; if neighbor digest
		// equals current, recurrence cannot prove standing vs current split.
		if obs.NeighborDigest == currentKey {
			return Decision{
				Kind:       DecisionClarificationRequired,
				ReasonCode: ReasonAmbiguousAdjacentUsers,
				Candidates: userCandidates(in.Segments),
				Evidence: []Evidence{{
					Class: EvidenceAmbiguous,
					Code:  "unchanged_neighboring_intent",
				}},
			}, true
		}
	}
	// Relative suffix position must be stable (0 = last user, 1 = second-to-last).
	if obs.RelativeSuffixIndex != 1 {
		return Decision{
			Kind:       DecisionClarificationRequired,
			ReasonCode: ReasonAmbiguousAdjacentUsers,
			Candidates: userCandidates(in.Segments),
			Evidence: []Evidence{{
				Class: EvidenceAmbiguous,
				Code:  "unstable_suffix_position",
			}},
		}, true
	}

	var standing, current []string
	var evidence []Evidence
	for _, seg := range in.Segments {
		switch {
		case seg.ID == standingCand.ID:
			standing = append(standing, seg.ID)
			evidence = append(evidence, Evidence{Class: EvidenceRecurrence, Code: "recurring_standing", Segment: seg.ID})
		case seg.ID == currentCand.ID:
			current = append(current, seg.ID)
			evidence = append(evidence, Evidence{Class: EvidenceRecurrence, Code: "novel_current", Segment: seg.ID})
		case seg.Role == RoleSystem || seg.Role == RoleDeveloper:
			standing = append(standing, seg.ID)
		}
	}
	return Decision{
		Kind:             DecisionResolvedRecurrence,
		CurrentSegments:  current,
		StandingSegments: standing,
		Evidence:         evidence,
		ReasonCode:       ReasonRecurringStanding,
		Candidates:       userCandidates(in.Segments),
	}, true
}

func findObservation(obs []RecurrenceObservation, digest string) *RecurrenceObservation {
	for i := range obs {
		if obs[i].SegmentDigest == digest {
			return &obs[i]
		}
	}
	return nil
}
