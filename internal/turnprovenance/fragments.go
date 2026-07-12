package turnprovenance

import "sort"

// resolveFragments merges user segments only when structural fragment evidence
// is complete. Empty separators alone are never sufficient.
func resolveFragments(segments []Segment) (Decision, bool) {
	groups := map[string][]Segment{}
	for _, seg := range segments {
		if seg.Role != RoleUser || seg.FragmentGroupID == "" {
			continue
		}
		groups[seg.FragmentGroupID] = append(groups[seg.FragmentGroupID], seg)
	}
	if len(groups) == 0 {
		return Decision{}, false
	}

	var mergeGroups [][]string
	var current []string
	var evidence []Evidence
	complete := true

	for groupID, members := range groups {
		sort.Slice(members, func(i, j int) bool {
			ii, jj := -1, -1
			if members[i].FragmentIndex != nil {
				ii = *members[i].FragmentIndex
			}
			if members[j].FragmentIndex != nil {
				jj = *members[j].FragmentIndex
			}
			return ii < jj
		})
		if !fragmentGroupComplete(members) {
			complete = false
			evidence = append(evidence, Evidence{
				Class:  EvidenceAmbiguous,
				Code:   "incomplete_fragment_group",
				Detail: groupID,
			})
			continue
		}
		ids := make([]string, 0, len(members))
		for _, m := range members {
			ids = append(ids, m.ID)
			evidence = append(evidence, Evidence{Class: EvidenceStructural, Code: "fragment_member", Segment: m.ID, Detail: groupID})
		}
		mergeGroups = append(mergeGroups, ids)
		// The ordered group is one current intent.
		current = append(current, ids...)
	}

	if !complete {
		return Decision{
			Kind:        DecisionClarificationRequired,
			Evidence:    evidence,
			ReasonCode:  ReasonIncompleteFragments,
			Candidates:  userCandidates(segments),
			MergeGroups: mergeGroups,
		}, true
	}
	if len(mergeGroups) == 0 {
		return Decision{}, false
	}

	var standing []string
	for _, seg := range segments {
		if seg.Role == RoleSystem || seg.Role == RoleDeveloper || seg.DeclaredProvenance == ProvenanceStanding {
			standing = append(standing, seg.ID)
		}
	}
	return Decision{
		Kind:             DecisionResolvedFragments,
		CurrentSegments:  current,
		StandingSegments: standing,
		MergeGroups:      mergeGroups,
		Evidence:         evidence,
		ReasonCode:       ReasonCompleteFragments,
		Candidates:       userCandidates(segments),
	}, true
}

func fragmentGroupComplete(members []Segment) bool {
	if len(members) == 0 {
		return false
	}
	count := -1
	seen := map[int]bool{}
	for _, m := range members {
		if m.FragmentIndex == nil || m.FragmentCount == nil {
			return false
		}
		if count < 0 {
			count = *m.FragmentCount
		}
		if *m.FragmentCount != count || count <= 0 {
			return false
		}
		idx := *m.FragmentIndex
		if idx < 0 || idx >= count || seen[idx] {
			return false
		}
		seen[idx] = true
	}
	return len(seen) == count
}

// AmbiguousTrailingUsers is the structural detector formerly used as final policy.
// It reports whether two non-empty unspecified user segments occupy the turn suffix.
func AmbiguousTrailingUsers(segments []Segment) bool {
	users := trailingUserSegments(segments)
	if len(users) < 2 {
		return false
	}
	for _, seg := range users[len(users)-2:] {
		if seg.DeclaredProvenance != ProvenanceUnspecified {
			return false
		}
		if seg.ByteLength == 0 && !seg.HasImages {
			return false
		}
	}
	return true
}
